// Package links shows the model the pages a message links to: the URLs in
// the addressed message, or in the message it replies to, are fetched and
// their text added to the request. Only URLs a person sent are fetched,
// never ones the model or a page names, and never a private address.
// History keeps only a note.
package links

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

const (
	maxLinks     = 5        // URLs fetched a turn
	maxBody      = 5 << 20  // bytes read of a page
	maxPDF       = 12 << 20 // bytes read of a PDF
	maxText      = 15_000   // characters of a page's text shown
	minArticle   = 200      // characters an article or main element needs to stand for its page
	maxRedirects = 3
	timeout      = 15 * time.Second
	userAgent    = "signalbot (+https://github.com/robdevops/signalbot)"
	maxPerHour   = 20 // links fetched for one chat in an hour
)

// now is the clock the per-chat cap reads; tests replace it.
var now = time.Now

// privateParams are query parameter names, lowercased and without - or _,
// that carry a secret or a one-time token; a name ending in signature or
// token does too (X-Amz-Signature, access_token). Names common in ordinary
// links (code, session, hash, ticket) are left out.
var privateParams = map[string]bool{"token": true, "accesstoken": true, "authtoken": true, "key": true, "apikey": true,
	"secret": true, "password": true, "passwd": true, "pwd": true, "sig": true, "signature": true, "otp": true,
	"sessionid": true, "jwt": true}

// actionWords are path segments, whole, of links that do something when
// opened or are someone's one-time link. Pages that are harmless to open
// without being signed in, which the bot never is (login, logout, oauth),
// are left out, and so are words common in ordinary paths (hooks).
var actionWords = map[string]bool{"unsubscribe": true, "confirm": true, "verify": true, "verification": true,
	"reset": true, "magic": true, "activate": true, "delete": true, "remove": true, "approve": true,
	"webhook": true, "webhooks": true, "trigger": true}

// private reports whether u looks like a private or one-time link, which
// fetching could spend or act on: a password reset or unsubscribe link, a
// webhook, or one carrying a token, key or signature.
func private(u *url.URL) bool {
	for name := range u.Query() {
		n := strings.NewReplacer("-", "", "_", "").Replace(strings.ToLower(name))
		if privateParams[n] || strings.HasSuffix(n, "signature") || strings.HasSuffix(n, "token") {
			return true
		}
	}
	for _, seg := range strings.Split(strings.ToLower(u.Path), "/") {
		if actionWords[seg] {
			return true
		}
	}
	return false
}

// limiter caps the links fetched for each chat in an hour, in memory: the
// times of each chat's recent fetches, never their URLs.
type limiter struct {
	mu     sync.Mutex
	recent map[string][]time.Time
}

// allow reports whether the chat may have another link fetched, and if so
// counts it; if not, wait is how long until it may.
func (l *limiter) allow(chat string) (ok bool, wait time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := now()
	for c, times := range l.recent { // chats idle for an hour leave nothing behind
		if c != chat && t.Sub(times[len(times)-1]) >= time.Hour {
			delete(l.recent, c)
		}
	}
	times := slices.DeleteFunc(l.recent[chat], func(at time.Time) bool { return t.Sub(at) >= time.Hour })
	if len(times) >= maxPerHour {
		l.recent[chat] = times
		return false, times[len(times)-maxPerHour].Add(time.Hour).Sub(t)
	}
	l.recent[chat] = append(times, t)
	return true, 0
}

// urlPattern finds URLs in text, ending at white space, a Unicode separator
// or a control or format character (a zero-width space, say); trailing
// punctuation is trimmed after.
var urlPattern = regexp.MustCompile("https?://[^\\s\\p{Z}\\p{C}<>\"'`]+")

// Register adds the pages a message links to to the model's request on h.
func Register(h feature.Host) error {
	c, l := newClient(), &limiter{recent: map[string][]time.Time{}}
	h.OnTurn(func(ctx context.Context, m feature.Message) feature.Additions {
		return additions(ctx, c, l, h.Info(), m)
	})
	return nil
}

// urls is the distinct http and https URLs in m's text, then its quote's,
// at most maxLinks, and how many more distinct ones there were. A quote of
// the bot's own message is the model's words, so its URLs are not fetched.
func urls(m feature.Message) (out []string, dropped int) {
	text := m.Text
	if m.Quote != nil && !m.Quote.Bot {
		text += "\n" + m.Quote.Text
	}
	for _, raw := range urlPattern.FindAllString(text, -1) {
		raw = trim(raw)
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || u.User != nil {
			continue
		}
		u.Fragment = ""
		if s := u.String(); !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out[:min(len(out), maxLinks)], max(0, len(out)-maxLinks)
}

// trim drops punctuation that ends a sentence rather than the URL: a
// closing bracket only when it has no opening one in the URL.
func trim(u string) string {
	for u != "" {
		last := u[len(u)-1]
		switch {
		case strings.IndexByte(".,;:!?*_~", last) >= 0:
		case last == ')' && strings.Count(u, "(") < strings.Count(u, ")"):
		case last == ']' && strings.Count(u, "[") < strings.Count(u, "]"):
		default:
			return u
		}
		u = u[:len(u)-1]
	}
	return u
}

// outcome is what a link came to: the note's subject ("link <url>", or
// "page <url>" for one the model is shown) and its status after that, and
// what the model is shown of it, if anything.
type outcome struct {
	subject, status string
	text            string
	file            *feature.File
}

// shown reports whether the model is shown what the link says.
func (o outcome) shown() bool { return o.text != "" || o.file != nil }

// note is the whole note the model gets, with the page's text under it.
func (o outcome) note() string {
	n := "[" + o.subject + o.status + "]"
	if o.text != "" {
		n += "\n" + txt.Fence(strings.TrimSpace(o.text), maxText)
	}
	return n
}

// kept is the note history keeps: never what the page said.
func (o outcome) kept(u string) string {
	if o.shown() {
		return "[page " + u + " was shown to you" + feature.NotKept(1) + "]"
	}
	return "[" + o.subject + o.status + "]"
}

// additions fetches each URL in m and notes it, with its text or as a PDF
// file; history keeps a note only. Links past maxLinks are only counted, in
// a note to the model.
func additions(ctx context.Context, c *http.Client, l *limiter, info feature.Info, m feature.Message) feature.Additions {
	var notes, kept []string
	var files []feature.File
	content := false // the model gets what a page says
	found, dropped := urls(m)
	for _, u := range found {
		var o outcome
		switch parsed, _ := url.Parse(u); {
		case !webPort(parsed):
			logx.Infof("link not fetched: not a standard web port")
			o = outcome{subject: "link " + u, status: " not fetched: it isn't on a standard web port"}
		case private(parsed):
			logx.Infof("link not fetched: looks private or one-time")
			o = outcome{subject: "link " + u, status: " not fetched: it looks like a private or one-time link"}
		default:
			if ok, wait := l.allow(m.Chat.Key()); !ok {
				logx.Warnf("link not fetched: this chat reached its %d links an hour", maxPerHour)
				o = outcome{subject: "link " + u, status: fmt.Sprintf(" not fetched: this chat has had its %d links for the hour; "+
					"links can be read again in about %s", maxPerHour, minutes(wait))}
			} else {
				o = fetch(ctx, c, info, u)
			}
		}
		notes, kept = append(notes, o.note()), append(kept, o.kept(u))
		content = content || o.shown()
		if o.file != nil {
			files = append(files, *o.file)
		}
	}
	if dropped > 0 {
		logx.Infof("%d more links not opened: more than %d in a message", dropped, maxLinks)
		more := fmt.Sprintf("[%d more links not opened: the bot reads at most %d a message; send them on their own to have them read]", dropped, maxLinks)
		if dropped == 1 {
			more = fmt.Sprintf("[1 more link not opened: the bot reads at most %d a message; send it on its own to have it read]", maxLinks)
		}
		notes = append(notes, more) // history keeps the message, which has the URL
	}
	if len(notes) == 0 {
		return feature.Additions{}
	}
	return feature.Additions{Note: strings.Join(notes, "\n"), HistoryNote: strings.Join(kept, "\n"), Files: files, Content: content}
}

// minutes is d in whole minutes, rounded up, at least 1.
func minutes(d time.Duration) string {
	if n := max(1, int((d+time.Minute-1)/time.Minute)); n > 1 {
		return strconv.Itoa(n) + " minutes"
	}
	return "1 minute"
}

// fetch gets u, and what it came to. It logs one line, never the URL or
// the page's title.
func fetch(ctx context.Context, c *http.Client, info feature.Info, u string) outcome {
	failed := func(secs float64, why string) outcome {
		logx.Warnf("link failed after %.1fs: %s", secs, why)
		return outcome{subject: "link " + u, status: " couldn't be fetched: " + why}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		logx.Warnf("link failed: not a web address")
		return outcome{subject: "link " + u, status: " couldn't be fetched: not a web address"}
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,application/json;q=0.9,application/pdf;q=0.8,*/*;q=0.5")
	start := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		return failed(time.Since(start).Seconds(), reason(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return failed(time.Since(start).Seconds(), statusReason(resp.StatusCode))
	}
	// shown is the link as the note names it: with where it ended up, if
	// redirected. The redirecting server chose that, so it is cut short and
	// shown without any user name or password.
	final := *resp.Request.URL
	final.User = nil
	shown := u
	if f := final.String(); f != u {
		shown = u + ", redirected to " + txt.Clamp(f, 200)
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	limit := int64(maxBody)
	if mediaType == "application/pdf" {
		limit = maxPDF
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return failed(time.Since(start).Seconds(), reason(err))
	}
	// done logs the link's time, size, kind and outcome. The kind is one of
	// a fixed set: the server's own media type is its to choose, so it is
	// never written as it came.
	took, size, typ := time.Since(start).Seconds(), len(body), "other"
	switch top, _, _ := strings.Cut(mediaType, "/"); {
	case mediaType == "":
		typ = "unknown type"
	case mediaType == "application/json":
		typ = "json"
	case top == "text":
		typ = "text"
	case top == "image", top == "audio", top == "video":
		typ = top
	}
	done := func(warn bool, kind, outcome string) {
		f := logx.Infof
		if warn {
			f = logx.Warnf
		}
		f("link %.1fs, %d bytes %s, %s", took, size, kind, outcome)
	}
	link, page := "link "+shown, "page "+shown
	switch {
	case bytes.HasPrefix(body, []byte("%PDF-")):
		if !info.PDFs {
			done(false, "pdf", "not sent: this model can't read PDFs")
			return outcome{subject: link, status: ", a PDF, which you can't read with this model"}
		}
		done(false, "pdf", "attached")
		return outcome{subject: page, status: ", a PDF, attached",
			file: &feature.File{Name: cmp.Or(txt.SafeName(path.Base(resp.Request.URL.Path)), "page.pdf"), MIME: "application/pdf", Data: body}}
	case mediaType == "text/html" || mediaType == "application/xhtml+xml":
		title, text, from := pageText(body, resp.Header.Get("Content-Type"))
		if text = strings.TrimSpace(text); text == "" {
			done(true, "html", "no readable text")
			return outcome{subject: link, status: ", a page with no readable text"}
		}
		done(false, "html", txt.Chars(utf8.RuneCountInString(text), maxText)+" ("+from+")")
		return outcome{subject: page, status: titled(title), text: text}
	case strings.HasPrefix(mediaType, "text/") || mediaType == "application/json":
		r, err := charset.NewReader(bytes.NewReader(body), resp.Header.Get("Content-Type"))
		if err == nil {
			body, _ = io.ReadAll(r)
		}
		text := strings.TrimSpace(string(body))
		if text == "" {
			done(true, typ, "no readable text")
			return outcome{subject: link, status: ", a page with no readable text"}
		}
		if utf8.ValidString(text) && !strings.ContainsRune(text, 0) {
			done(false, typ, txt.Chars(utf8.RuneCountInString(text), maxText))
			return outcome{subject: page, text: text}
		}
	}
	done(false, typ, "not a page the bot reads")
	return outcome{subject: link, status: ", which can't be read"}
}

// statusReason is an error status in plain words, keeping its number.
func statusReason(code int) string {
	n := strconv.Itoa(code)
	switch {
	case code == http.StatusUnauthorized, code == http.StatusForbidden:
		return "the site refused the request (" + n + "); it may need signing in or block bots"
	case code == http.StatusNotFound, code == http.StatusGone:
		return "the page wasn't found (" + n + ")"
	case code == http.StatusTooManyRequests:
		return "the site is limiting requests (" + n + "); try again later"
	case code >= 500 && code <= 599:
		return "the site had an error (" + n + ")"
	}
	return "the site answered with status " + n
}

// reason is why a request failed, for the model and the log: never the
// URL, which can hold anything.
func reason(err error) string {
	switch {
	case errors.Is(err, errBlocked):
		return "it isn't a public address"
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(err.Error(), "Client.Timeout"):
		return "timed out"
	case errors.Is(err, errPrivateRedirect):
		return "redirected to a private or one-time link"
	case errors.Is(err, errPortRedirect):
		return "redirected to a non-standard port"
	case strings.Contains(err.Error(), "redirect"):
		return "too many or bad redirects"
	}
	return "couldn't connect"
}

func titled(title string) string {
	if title = txt.Clamp(strings.Join(strings.Fields(title), " "), 120); title != "" {
		return ": " + strconv.Quote(title)
	}
	return ""
}

// skipped are elements whose text is no part of a page's content.
var skipped = map[string]bool{"script": true, "style": true, "noscript": true, "template": true, "svg": true,
	"nav": true, "header": true, "footer": true, "aside": true, "form": true, "iframe": true, "button": true}

// block are elements that start a new line.
var block = map[string]bool{"p": true, "div": true, "br": true, "li": true, "h1": true, "h2": true, "h3": true,
	"h4": true, "h5": true, "h6": true, "tr": true, "section": true, "article": true, "blockquote": true,
	"pre": true, "table": true, "ul": true, "ol": true, "dd": true, "dt": true, "main": true, "hr": true}

// pageText is an HTML page's title and its text, a line per block, with
// navigation, scripts and forms left out: the text of its first article or
// main element if that has minArticle characters or more, else the whole
// page's, so menus and sidebars built of plain divs don't crowd out the
// content; from says which, "<article>", "<main>" or "whole page", for the
// log. It reads the page as a stream of tokens, never building the whole
// tree, and stops once it has enough text.
func pageText(body []byte, contentType string) (title, text, from string) {
	r, err := charset.NewReader(bytes.NewReader(body), contentType)
	if err != nil {
		r = bytes.NewReader(body)
	}
	z := html.NewTokenizer(r)
	var b, art strings.Builder // the page's text, and its article's
	skip, inTitle := 0, false  // skip: depth inside elements left out
	artTag, artDepth, artDone := "", 0, false
	newline := func() {
		for _, w := range []*strings.Builder{&b, &art} {
			if (w == &b || artDepth > 0) && w.Len() > 0 && !strings.HasSuffix(w.String(), "\n") {
				w.WriteByte('\n')
			}
		}
	}
	result := func() (string, string, string) {
		if a := squeeze(art.String()); len(a) >= minArticle {
			return title, a, "<" + artTag + ">"
		}
		return title, squeeze(b.String()), "whole page"
	}
	for b.Len() <= maxText*2 {
		switch tt := z.Next(); tt {
		case html.ErrorToken:
			return result()
		case html.StartTagToken, html.SelfClosingTagToken:
			name, _ := z.TagName()
			tag := string(name)
			switch {
			case skipped[tag]:
				if tt == html.StartTagToken {
					skip++
				}
			case tag == "title":
				inTitle = true
			case block[tag]:
				newline()
			}
			if tt == html.StartTagToken && skip == 0 && !artDone && (tag == artTag || artTag == "" && (tag == "article" || tag == "main")) {
				artTag, artDepth = tag, artDepth+1
			}
		case html.EndTagToken:
			name, _ := z.TagName()
			tag := string(name)
			switch {
			case skipped[tag] && skip > 0:
				skip--
			case tag == "title":
				inTitle = false
			case block[tag]:
				newline()
			}
			if tag == artTag && artDepth > 0 {
				if artDepth--; artDepth == 0 {
					artDone = true
				}
			}
		case html.TextToken:
			// One text node can be the whole 5 MB page: only as much as
			// the cap can use is split into words, kept as a title or
			// written, so the page's text is never copied whole.
			t := strings.Join(strings.Fields(string(head(z.Text(), maxText*4))), " ")
			switch {
			case t == "":
			case inTitle:
				if title == "" {
					title = head(t, 1000)
				}
			case skip == 0:
				for _, w := range []*strings.Builder{&b, &art} {
					if w == &art && artDepth == 0 {
						continue
					}
					if w.Len() > 0 && !strings.HasSuffix(w.String(), "\n") {
						w.WriteByte(' ')
					}
					w.WriteString(head(t, max(0, maxText*2+1-w.Len())))
				}
			}
		}
	}
	return result()
}

// head is s cut to at most n bytes, on a rune boundary.
func head[S ~string | ~[]byte](s S, n int) S {
	if n >= len(s) {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// squeeze drops blank lines and the spaces around lines.
func squeeze(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

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
	"github.com/robdevops/signalbot/internal/kit/img"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

const (
	maxLinks     = 2       // URLs fetched a turn
	maxBody      = 5 << 20 // bytes read of a page
	maxText      = 15_000  // characters of a page's text shown
	minArticle   = 200     // characters an article or main element needs to stand for its page
	maxRedirects = 3
	timeout      = 15 * time.Second
	userAgent    = "signalbot (+https://github.com/robdevops/signalbot)"
	maxPerHour   = 20 // links fetched for one chat in an hour
)

// now is the clock the per-chat cap reads; tests replace it.
var now = time.Now

// privateParams are query parameter names, lowercased and without - or _,
// that carry a secret or a one-time token; a name ending in signature,
// token or key does too (X-Amz-Signature, access_token, api_key).
var privateParams = map[string]bool{"token": true, "accesstoken": true, "authtoken": true, "key": true, "apikey": true,
	"secret": true, "password": true, "passwd": true, "pwd": true, "sig": true, "signature": true, "auth": true,
	"code": true, "otp": true, "session": true, "sessionid": true, "jwt": true, "ticket": true, "hash": true}

// actionWords start the path segments of links that do something when
// opened, or are someone's one-time link.
var actionWords = []string{"unsubscribe", "confirm", "verify", "verification", "reset", "login", "logout", "signin",
	"signout", "magic", "activate", "delete", "remove", "approve", "webhook", "hooks", "trigger", "callback",
	"oauth", "authorize"}

// private reports whether u looks like a private or one-time link, which
// fetching could spend or act on: a password reset, a login or unsubscribe
// link, a webhook, or one carrying a token, key or signature.
func private(u *url.URL) bool {
	for name := range u.Query() {
		n := strings.NewReplacer("-", "", "_", "").Replace(strings.ToLower(name))
		if privateParams[n] || strings.HasSuffix(n, "signature") || strings.HasSuffix(n, "token") || strings.HasSuffix(n, "key") {
			return true
		}
	}
	for _, seg := range strings.Split(strings.ToLower(u.Path), "/") {
		for _, w := range actionWords {
			if strings.HasPrefix(seg, w) {
				return true
			}
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
// counts it.
func (l *limiter) allow(chat string) bool {
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
		return false
	}
	l.recent[chat] = append(times, t)
	return true
}

// urlPattern finds URLs in text; trailing punctuation is trimmed after.
var urlPattern = regexp.MustCompile("https?://[^\\s<>\"'`]+")

// Register adds the pages a message links to to the model's request on h.
func Register(h feature.Host) error {
	c, l := newClient(), &limiter{recent: map[string][]time.Time{}}
	h.OnTurn(func(ctx context.Context, m feature.Message) feature.Additions {
		return additions(ctx, c, l, h.Info(), m)
	})
	return nil
}

// urls is the distinct http and https URLs in m's text, then its quote's,
// at most maxLinks. A quote of the bot's own message is the model's words,
// so its URLs are not fetched.
func urls(m feature.Message) []string {
	text := m.Text
	if m.Quote != nil && !m.Quote.Bot {
		text += "\n" + m.Quote.Text
	}
	var out []string
	for _, raw := range urlPattern.FindAllString(text, -1) {
		raw = trim(raw)
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || u.User != nil || len(out) == maxLinks {
			continue
		}
		u.Fragment = ""
		if s := u.String(); !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
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

// additions fetches each URL in m and notes it, with its text or as a PDF
// file; history keeps a note only.
func additions(ctx context.Context, c *http.Client, l *limiter, info feature.Info, m feature.Message) feature.Additions {
	var notes, kept []string
	var files []feature.File
	for _, u := range urls(m) {
		var note, text string
		var file *feature.File
		switch parsed, _ := url.Parse(u); {
		case private(parsed):
			note = "[link " + u + " not fetched: it looks like a private or one-time link"
		case !l.allow(m.Chat.Key()):
			note = "[link " + u + " not fetched: too many links this hour; try again later"
		default:
			note, text, file = fetch(ctx, c, info, u)
		}
		kept = append(kept, note+"]")
		if text != "" || file != nil {
			kept[len(kept)-1] = "[page " + u + " was shown to you; it isn't kept]"
		}
		if text != "" {
			note += "]\n" + txt.Fence(strings.TrimSpace(text), maxText)
		} else {
			note += "]"
		}
		notes = append(notes, note)
		if file != nil {
			files = append(files, *file)
		}
	}
	if len(notes) == 0 {
		return feature.Additions{}
	}
	return feature.Additions{Note: strings.Join(notes, "\n"), HistoryNote: strings.Join(kept, "\n"), Files: files}
}

// fetch gets u: its note, open for the caller to close with "]", and its
// text or, for a PDF, the file.
func fetch(ctx context.Context, c *http.Client, info feature.Info, u string) (note, text string, file *feature.File) {
	failed := func(why string) (string, string, *feature.File) {
		return "[link " + u + " failed to load: " + why, "", nil
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return failed("not a web address")
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,application/json;q=0.9,application/pdf;q=0.8,*/*;q=0.5")
	start := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		logx.Warnf("link failed after %.1fs: %v", time.Since(start).Seconds(), reason(err))
		return failed(reason(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return failed("status " + strconv.Itoa(resp.StatusCode))
	}
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	limit := int64(maxBody)
	if mediaType == "application/pdf" {
		limit = img.MaxReceived
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return failed(reason(err))
	}
	logx.Infof("link %.1fs, %d bytes", time.Since(start).Seconds(), len(body))
	switch {
	case bytes.HasPrefix(body, []byte("%PDF-")):
		if !info.Files {
			return "[link " + u + ", a PDF, which you can't read with this model", "", nil
		}
		return "[page " + u + ", a PDF, attached", "", &feature.File{Name: cmp.Or(txt.SafeName(path.Base(resp.Request.URL.Path)), "page.pdf"), MIME: "application/pdf", Data: body}
	case mediaType == "text/html" || mediaType == "application/xhtml+xml":
		title, text := pageText(body, resp.Header.Get("Content-Type"))
		if strings.TrimSpace(text) == "" {
			return "[link " + u + ", a page with no text you can read", "", nil
		}
		return "[page " + u + titled(title), text, nil
	case strings.HasPrefix(mediaType, "text/") || mediaType == "application/json":
		r, err := charset.NewReader(bytes.NewReader(body), resp.Header.Get("Content-Type"))
		if err == nil {
			body, _ = io.ReadAll(r)
		}
		if utf8.Valid(body) && !bytes.ContainsRune(body, 0) {
			return "[page " + u, string(body), nil
		}
	}
	return "[link " + u + ", which isn't a page you can read", "", nil
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
// content. It reads the page as a stream of tokens, never building the
// whole tree, and stops once it has enough text.
func pageText(body []byte, contentType string) (title, text string) {
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
	result := func() (string, string) {
		if a := squeeze(art.String()); len(a) >= minArticle {
			return title, a
		}
		return title, squeeze(b.String())
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
			t := strings.Join(strings.Fields(string(z.Text())), " ")
			switch {
			case t == "":
			case inTitle:
				if title == "" {
					title = t
				}
			case skip == 0:
				for _, w := range []*strings.Builder{&b, &art} {
					if w == &art && artDepth == 0 {
						continue
					}
					if w.Len() > 0 && !strings.HasSuffix(w.String(), "\n") {
						w.WriteByte(' ')
					}
					w.WriteString(t)
				}
			}
		}
	}
	return result()
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

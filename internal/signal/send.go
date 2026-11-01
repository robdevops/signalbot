package signal

import (
	"encoding/base64"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

const (
	// maxChars is Signal's limit for one message; replies are split at it.
	maxChars = 2000
	// maxReply caps a whole reply, so one request can't fan out into
	// hundreds of messages.
	maxReply  = 4 * maxChars
	truncated = " […truncated]"
)

// Prompt is Signal's line in the system prompt.
func (c *Client) Prompt() string {
	return fmt.Sprintf("This is a Signal chat. Never exceed %d characters, which is Signal's limit for one message.", maxChars)
}

// Send posts text to chat as parts makes it, with each @Name in known
// (lowercased name to ID), and in a group the bot's own @name, made a real mention, and
// the images on the first
// message, each named after its Name or else image_<local time>. A failure
// is logged, as is a message signal-cli says it could not deliver.
func (c *Client) Send(chat feature.Chat, text string, pics []feature.Picture, known map[string]string) {
	if n := utf8.RuneCountInString(text); n > maxReply {
		logx.Warnf("reply truncated from %d chars", n)
	}
	var attachments []string
	stamp := time.Now().Format("20060102-150405")
	for i, p := range pics { // with the extension their type gives
		name := fileName(p.Name)
		if name == "" {
			name = "image_" + stamp
		}
		if i > 0 {
			name += fmt.Sprintf("_%d", i+1)
		}
		mime := http.DetectContentType(p.Data)
		attachments = append(attachments, fmt.Sprintf("data:%s;filename=%s.%s;base64,%s",
			mime, name, strings.TrimPrefix(mime, "image/"), base64.StdEncoding.EncodeToString(p.Data)))
	}
	ps := parts(text, c.BotName())
	if id := c.self(); id != "" && chat.GroupID != "" { // in a group, "@" and its own name mentions the bot
		k := maps.Clone(known)
		if k == nil {
			k = map[string]string{}
		}
		k[txt.SafeName(c.BotName())] = id
		known = k
	}
	if len(ps) == 0 {
		if len(attachments) == 0 {
			logx.Warnf("empty reply, nothing sent")
			return
		}
		ps = []styled{{}}
	}
	for i, part := range ps {
		if i > 0 {
			attachments = nil
		}
		text := string(part.text)
		if err := c.send(chat, text, mentions(text, known), textStyles(part), attachments); err != nil {
			logx.Errorf("send failed: %v", err)
			return
		}
	}
}

// fileName is name made safe as a file name in a data URI: characters that
// file systems or the URI reserve are dropped, spaces are collapsed, and it is
// cut to 80 characters.
func fileName(name string) string {
	name = strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f || strings.ContainsRune(`/\:*?"<>|;,`, r) {
			return -1
		}
		return r
	}, name)
	r := []rune(strings.Join(strings.Fields(name), " "))
	return strings.TrimSpace(string(r[:min(len(r), 80)]))
}

// send posts one message.
func (c *Client) send(chat feature.Chat, text string, mentions, styles, attachments []string) error {
	p := target(chat)
	p["message"] = text
	if len(mentions) > 0 {
		p["mention"] = mentions
	}
	if len(styles) > 0 {
		p["textStyle"] = styles
	}
	if len(attachments) > 0 {
		p["attachments"] = attachments
	}
	return c.write(c.nextID.Add(1), "send", p)
}

func target(chat feature.Chat) map[string]any {
	if chat.GroupID != "" {
		return map[string]any{"groupId": chat.GroupID}
	}
	return map[string]any{"recipient": []string{chat.User}}
}

// Typing shows the bot typing in chat until stop is called.
func (c *Client) Typing(chat feature.Chat) (stop func()) {
	done := make(chan struct{})
	send := func() { _ = c.write(c.nextID.Add(1), "sendTyping", target(chat)) }
	send()
	go func() {
		tick := time.NewTicker(c.typingEvery)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				send()
			}
		}
	}()
	return sync.OnceFunc(func() { close(done) })
}

// parts turns a reply into the messages to send: its Markdown converted,
// without a leading @ and the bot's own name (an echo of how it was
// addressed; elsewhere it is a mention), capped as a whole at maxReply, and
// split at Signal's limit, preferring a line break and then a space. A style
// carries on across a split.
func parts(reply, self string) []styled {
	s := markdown(reply)
	if sp := findAt(s.text, map[string]string{txt.SafeName(self): ""}); len(sp) > 0 && strings.TrimSpace(string(s.text[:sp[0].start])) == "" {
		s.text, s.style = s.text[sp[0].end:], s.style[sp[0].end:]
	}
	if len(s.text) > maxReply {
		n := maxReply - utf8.RuneCountInString(truncated)
		s.text, s.style = s.text[:n], s.style[:n]
		s.add([]rune(truncated), 0)
	}
	var out []styled
	r, st := s.text, s.style
	for {
		for len(r) > 0 && unicode.IsSpace(r[0]) {
			r, st = r[1:], st[1:]
		}
		if len(r) == 0 {
			return out
		}
		end := min(len(r), maxChars)
		if end < len(r) {
			if i := lastIndex(r[:end], '\n'); i > 0 {
				end = i
			} else if i := lastIndex(r[:end], ' '); i > maxChars/2 {
				end = i
			}
		}
		n := end
		for unicode.IsSpace(r[n-1]) {
			n--
		}
		out = append(out, styled{r[:n:n], st[:n:n]})
		r, st = r[end:], st[end:]
	}
}

func lastIndex(r []rune, want rune) int {
	for i, v := range slices.Backward(r) {
		if v == want {
			return i
		}
	}
	return -1
}

// span is one "@name" in a text, as rune indices [start, end).
type span struct {
	start, end int
	id         string
}

// findAt finds "@name" for each name in known (name to ID). Matching
// ignores case, tries the longest name first, and sits on word boundaries.
func findAt(rs []rune, known map[string]string) []span {
	names := slices.DeleteFunc(slices.Collect(maps.Keys(known)), func(name string) bool { return name == "" })
	slices.SortFunc(names, func(a, b string) int { return utf8.RuneCountInString(b) - utf8.RuneCountInString(a) })
	var out []span
	for i := 0; i < len(rs); i++ {
		if rs[i] != '@' || (i > 0 && wordRune(rs[i-1])) {
			continue
		}
		for _, name := range names {
			end := i + 1 + utf8.RuneCountInString(name)
			if end > len(rs) || (end < len(rs) && wordRune(rs[end])) || strings.ToLower(string(rs[i+1:end])) != strings.ToLower(name) {
				continue
			}
			out = append(out, span{i, end, known[name]})
			i = end - 1
			break
		}
	}
	return out
}

// utf16At is the UTF-16 offset of each rune of rs, and then of its end.
func utf16At(rs []rune) []int {
	u := make([]int, len(rs)+1)
	for i, r := range rs {
		u[i+1] = u[i] + utf16.RuneLen(r)
	}
	return u
}

// mentions is the signal-cli mentions ("start:length:uuid", in UTF-16
// units) for each @Name in text whose name is in known (lower-cased name to
// ID). An ambiguous name, with no ID, stays plain text.
func mentions(text string, known map[string]string) []string {
	rs := []rune(text)
	u := utf16At(rs)
	var out []string
	for _, s := range findAt(rs, known) {
		if s.id != "" {
			out = append(out, fmt.Sprintf("%d:%d:%s", u[s.start], u[s.end]-u[s.start], s.id))
		}
	}
	return out
}

// strip removes "@name" for each of names, without leaving a double space,
// and reports whether it found one.
func strip(s styled, names ...string) (styled, bool) {
	known := map[string]string{} // findAt needs only the names
	for _, n := range names {
		known[txt.SafeName(n)] = ""
	}
	spans := findAt(s.text, known)
	if len(spans) == 0 {
		return s, false
	}
	var out styled
	pos := 0
	for _, sp := range spans {
		out.text = append(out.text, s.text[pos:sp.start]...)
		out.style = append(out.style, s.style[pos:sp.start]...)
		pos = sp.end
		if pos < len(s.text) && s.text[pos] == ' ' && (len(out.text) == 0 || out.text[len(out.text)-1] == ' ') {
			pos++
		}
	}
	out.text, out.style = append(out.text, s.text[pos:]...), append(out.style, s.style[pos:]...)
	for len(out.text) > 0 && unicode.IsSpace(out.text[0]) {
		out.text, out.style = out.text[1:], out.style[1:]
	}
	for n := len(out.text); n > 0 && unicode.IsSpace(out.text[n-1]); n-- {
		out.text, out.style = out.text[:n-1], out.style[:n-1]
	}
	return out, true
}

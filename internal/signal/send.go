package signal

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"sort"
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

// Send posts text to chat: without the bot's own @name, with each @Name in
// known (lowercased name to ID) made a real mention, cut short past maxReply
// with a warning, and split at Signal's limit, with the images on the first
// message. A failure is logged, and whether a message was delivered is
// logged when signal-cli says.
func (c *Client) Send(chat feature.Chat, text string, images [][]byte, known map[string]string) {
	if n := utf8.RuneCountInString(text); n > maxReply {
		logx.Warnf("reply truncated from %d chars", n)
	}
	var attachments []string
	for i, data := range images { // named with the extension their type gives
		mime := http.DetectContentType(data)
		attachments = append(attachments, fmt.Sprintf("data:%s;filename=image%d.%s;base64,%s",
			mime, i+1, strings.TrimPrefix(mime, "image/"), base64.StdEncoding.EncodeToString(data)))
	}
	ps := parts(text, c.BotName())
	if len(ps) == 0 {
		if len(attachments) == 0 {
			logx.Warnf("empty reply, nothing sent")
			return
		}
		ps = []string{""}
	}
	for i, part := range ps {
		if i > 0 {
			attachments = nil
		}
		if err := c.send(chat, part, mentions(part, known), attachments); err != nil {
			logx.Errorf("send failed: %v", err)
			return
		}
	}
}

// send posts one message, with mentions ("start:length:uuid") and
// attachments.
func (c *Client) send(chat feature.Chat, text string, mentions, attachments []string) error {
	p := target(chat)
	p["message"] = text
	if len(mentions) > 0 {
		p["mention"] = mentions
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

// parts turns a reply into the messages to send: without the bot's own
// @name, capped as a whole at maxReply, and split at Signal's limit,
// preferring a line break and then a space.
func parts(reply, self string) []string {
	reply, _ = stripAt(reply, self)
	r := []rune(reply)
	if len(r) > maxReply {
		r = append(r[:maxReply-utf8.RuneCountInString(truncated)], []rune(truncated)...)
	}
	var out []string
	for {
		for len(r) > 0 && unicode.IsSpace(r[0]) {
			r = r[1:]
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
		out = append(out, strings.TrimRightFunc(string(r[:end]), unicode.IsSpace))
		r = r[end:]
	}
}

func lastIndex(r []rune, want rune) int {
	for i := len(r) - 1; i >= 0; i-- {
		if r[i] == want {
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
	type cand struct {
		lower string
		n     int // runes
		id    string
	}
	var cands []cand
	for name, id := range known {
		if name != "" {
			cands = append(cands, cand{strings.ToLower(name), utf8.RuneCountInString(name), id})
		}
	}
	if len(cands) == 0 {
		return nil
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].n > cands[j].n })
	var out []span
	for i := 0; i < len(rs); i++ {
		if rs[i] != '@' || (i > 0 && wordRune(rs[i-1])) {
			continue
		}
		for _, k := range cands {
			end := i + 1 + k.n
			if end > len(rs) || (end < len(rs) && wordRune(rs[end])) || strings.ToLower(string(rs[i+1:end])) != k.lower {
				continue
			}
			out = append(out, span{i, end, k.id})
			i = end - 1
			break
		}
	}
	return out
}

func utf16Len(rs []rune) (n int) {
	for _, r := range rs {
		n += utf16.RuneLen(r)
	}
	return n
}

// mentions is the signal-cli mentions ("start:length:uuid", in UTF-16
// units) for each @Name in text whose name is in known (lower-cased name to
// ID). An ambiguous name, with no ID, stays plain text.
func mentions(text string, known map[string]string) []string {
	rs := []rune(text)
	var out []string
	u, pos := 0, 0 // the UTF-16 offset of rs[pos]
	for _, s := range findAt(rs, known) {
		u += utf16Len(rs[pos:s.start])
		n := utf16Len(rs[s.start:s.end])
		if s.id != "" {
			out = append(out, fmt.Sprintf("%d:%d:%s", u, n, s.id))
		}
		u += n
		pos = s.end
	}
	return out
}

// stripAt removes "@name" for each of names from text, without leaving a
// double space, and reports whether it found one.
func stripAt(text string, names ...string) (string, bool) {
	known := map[string]string{} // findAt needs only the names
	for _, n := range names {
		known[txt.SafeName(n)] = ""
	}
	rs := []rune(text)
	spans := findAt(rs, known)
	if len(spans) == 0 {
		return text, false
	}
	var out []rune
	pos := 0
	for _, s := range spans {
		out = append(out, rs[pos:s.start]...)
		pos = s.end
		if pos < len(rs) && rs[pos] == ' ' && (len(out) == 0 || out[len(out)-1] == ' ') {
			pos++
		}
	}
	return strings.TrimSpace(string(append(out, rs[pos:]...))), true
}

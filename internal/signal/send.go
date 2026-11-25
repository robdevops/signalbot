package signal

import (
	"context"
	"encoding/base64"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/mdstyle"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

const (
	// maxReply caps a reply. Past 2,000 bytes, signal-cli sends the rest as
	// a long-text attachment, as Signal's apps do, shown behind "Read more",
	// so a reply is always one message.
	maxReply  = 8000
	truncated = " […truncated]"
)

// Prompt is Signal's line in the system prompt.
func (c *Client) Prompt() string {
	return "This is a Signal chat. Past about 2000 characters, a message shows only its start until the reader taps Read more, so put the answer first."
}

// Send posts text to chat as one message, as prepare makes it, with each
// @Name in known (lowercased name to ID), and in a group the bot's own
// @name, made a real mention, and with the images, each named after its
// Name or else image_<local time>, quoting the bot's own message sent at
// replyTo if it isn't 0. A failure is logged, as is a message signal-cli
// says it could not deliver. Signal shows none of mk: no buttons, no
// keyboard, and no silent send.
func (c *Client) Send(chat feature.Chat, text string, pics []feature.Picture, known map[string]string, replyTo int64, audio []byte, _ feature.Markup) (sent []int64) {
	if n := utf8.RuneCountInString(text); n > maxReply {
		log.Warnf("reply truncated from %d chars", n)
	}
	var attachments []string
	var kept []attachment // for a reply that quotes them: signal-cli would give it thumbnails
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
		kept = append(kept, attachment{ContentType: mime, Filename: name + "." + strings.TrimPrefix(mime, "image/"), Size: len(p.Data), data: p.Data})
	}
	if len(audio) > 0 { // named plainly, so the file gives nothing away
		attachments = append(attachments, "data:audio/mpeg;filename=clip.mp3;base64,"+base64.StdEncoding.EncodeToString(audio))
	}
	msg, ok := prepare(text, c.BotName())
	known = c.withSelf(chat, known)
	if !ok && len(attachments) == 0 {
		log.Warnf("empty reply, nothing sent")
		return nil
	}
	body := string(msg.Runes)
	ts, err := c.send(chat, body, mentions(body, known), textStyles(msg), attachments, replyTo)
	if err != nil {
		log.Errorf("send failed: %v", err)
		return nil
	}
	if ts != 0 {
		sent = append(sent, ts)
		if self := c.self(); self != "" && len(kept) > 0 {
			for i := range kept {
				kept[i].ID = fmt.Sprintf("%s%d:%d", sentPrefix, ts, i)
			}
			c.recent.add(chat.Key(), self, ts, kept)
		}
	}
	return sent
}

// Buttons reports that Signal shows no Markup.
func (c *Client) Caps() feature.Caps {
	return feature.Caps{EditLimit: maxEdits, EditWindow: editWindow}
}

// Signal's apps show at most maxEdits edits of a message, and only within
// editWindow of its sending.
const (
	maxEdits   = 10
	editWindow = 24 * time.Hour
)

// Edit replaces the text of the bot's own message sent at sent, as Signal's
// apps edit one, keeping its timestamp; with pics, which an edit can't
// change, they and the text are sent as a new message instead. mk is left
// off, as Send leaves it.
func (c *Client) Edit(chat feature.Chat, sent int64, text string, pics []feature.Picture, known map[string]string, mk feature.Markup) []int64 {
	if len(pics) > 0 {
		return c.Send(chat, text, pics, known, 0, nil, mk)
	}
	msg, ok := prepare(text, c.BotName())
	if !ok {
		log.Warnf("empty edit, nothing sent")
		return nil
	}
	known = c.withSelf(chat, known)
	body := string(msg.Runes)
	if _, err := c.post(chat, body, mentions(body, known), textStyles(msg), nil, 0, sent); err != nil {
		log.Errorf("edit failed: %v", err)
		return nil
	}
	return []int64{sent}
}

// withSelf is known with, in a group, the bot's own name for its account,
// so "@" and its name mentions it.
func (c *Client) withSelf(chat feature.Chat, known map[string]string) map[string]string {
	if id := c.self(); id != "" && chat.GroupID != "" {
		k := maps.Clone(known)
		if k == nil {
			k = map[string]string{}
		}
		k[txt.SafeName(c.BotName())] = id
		return k
	}
	return known
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

// send posts one message and returns its timestamp, which names it on
// Signal (for a delete from a linked device), waiting up to a minute for
// signal-cli; a recipient it wasn't delivered to is logged. With quote, it
// replies to the bot's own message sent then: a quote names its message by
// author and timestamp alone, and signal-cli sends it whether or not it
// still has the message, so a recipient whose app doesn't shows the quote
// as not found, and the reply still goes.
func (c *Client) send(chat feature.Chat, text string, mentions, styles, attachments []string, quote int64) (int64, error) {
	return c.post(chat, text, mentions, styles, attachments, quote, 0)
}

// post is send, and with edit an edit of the bot's own message sent then
// (signal-cli's editTimestamp).
func (c *Client) post(chat feature.Chat, text string, mentions, styles, attachments []string, quote, edit int64) (int64, error) {
	p := target(chat)
	p["message"] = text
	if self := c.self(); quote != 0 && self != "" {
		p["quoteTimestamp"], p["quoteAuthor"] = quote, self
	}
	if edit != 0 {
		p["editTimestamp"] = edit
	}
	if len(mentions) > 0 {
		p["mention"] = mentions
	}
	if len(styles) > 0 {
		p["textStyle"] = styles
	}
	if len(attachments) > 0 {
		p["attachments"] = attachments
	}
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()
	var r struct {
		Timestamp int64 `json:"timestamp"`
		Results   []struct {
			Type string `json:"type"`
		} `json:"results"`
	}
	if err := c.call(ctx, "send", p, &r); err != nil {
		return 0, err
	}
	for _, res := range r.Results {
		if res.Type != "" && res.Type != "SUCCESS" {
			log.Warnf("signal-cli send not delivered: %s", txt.SafeName(res.Type))
		}
	}
	return r.Timestamp, nil
}

// sendTimeout bounds one send: signal-cli answers once the message is out,
// which takes longer with attachments.
var sendTimeout = time.Minute

// React puts emoji on msg, as a reaction from the bot. The bot's own
// message is its account's, so not until signal-cli says which that is.
func (c *Client) React(chat feature.Chat, msg feature.Message, emoji string) {
	author := msg.Sender
	if msg.Bot {
		if author = c.self(); author == "" {
			log.Errorf("reaction failed: the bot's account isn't known yet")
			return
		}
	}
	p := target(chat)
	p["emoji"], p["targetAuthor"], p["targetTimestamp"] = emoji, author, msg.Timestamp
	if err := c.write(c.nextID.Add(1), "sendReaction", p); err != nil {
		log.Errorf("reaction failed: %v", err)
	}
}

func target(chat feature.Chat) map[string]any {
	if chat.GroupID != "" {
		return map[string]any{"groupId": chat.GroupID}
	}
	return map[string]any{"recipient": []string{chat.User}}
}

// Typing shows the bot typing in chat until stop is called, which says it
// stopped.
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
	// Stopping tells Signal too, or "typing…" stays up after a reply that
	// isn't a message, such as a reaction, or after none at all.
	return sync.OnceFunc(func() {
		close(done)
		p := target(chat)
		p["stop"] = true
		_ = c.write(c.nextID.Add(1), "sendTyping", p)
	})
}

// prepare turns a reply into the message to send: its Markdown converted,
// without a leading @ and the bot's own name (an echo of how it was
// addressed; elsewhere it is a mention), trimmed and capped at maxReply;
// false if nothing is left.
func prepare(reply, self string) (mdstyle.Text, bool) {
	s := mdstyle.Parse(reply, false)
	if sp := mdstyle.FindAt(s.Runes, map[string]string{txt.SafeName(self): ""}); len(sp) > 0 && strings.TrimSpace(string(s.Runes[:sp[0].Start])) == "" {
		s.Runes, s.Styles = s.Runes[sp[0].End:], s.Styles[sp[0].End:]
	}
	start, end := 0, len(s.Runes)
	for start < end && unicode.IsSpace(s.Runes[start]) {
		start++
	}
	for end > start && unicode.IsSpace(s.Runes[end-1]) {
		end--
	}
	s.Runes, s.Styles = s.Runes[start:end:end], s.Styles[start:end:end]
	if len(s.Runes) > maxReply {
		n := maxReply - utf8.RuneCountInString(truncated)
		s.Runes, s.Styles = s.Runes[:n:n], s.Styles[:n:n]
		s.Add([]rune(truncated), 0)
	}
	return s, len(s.Runes) > 0
}

// mentions is the signal-cli mentions ("start:length:uuid", in UTF-16
// units) for each @Name in text whose name is in known (lower-cased name to
// ID). An ambiguous name, with no ID, stays plain text.
func mentions(text string, known map[string]string) []string {
	rs := []rune(text)
	u := mdstyle.UTF16At(rs)
	var out []string
	for _, s := range mdstyle.FindAt(rs, known) {
		if s.ID != "" {
			out = append(out, fmt.Sprintf("%d:%d:%s", u[s.Start], u[s.End]-u[s.Start], s.ID))
		}
	}
	return out
}

// strip removes "@name" for each of names, without leaving a double space,
// and reports whether it found one.
func strip(s mdstyle.Text, names ...string) (mdstyle.Text, bool) {
	known := map[string]string{} // findAt needs only the names
	for _, n := range names {
		known[txt.SafeName(n)] = ""
	}
	spans := mdstyle.FindAt(s.Runes, known)
	if len(spans) == 0 {
		return s, false
	}
	var out mdstyle.Text
	pos := 0
	for _, sp := range spans {
		out.Runes = append(out.Runes, s.Runes[pos:sp.Start]...)
		out.Styles = append(out.Styles, s.Styles[pos:sp.Start]...)
		pos = sp.End
		if pos < len(s.Runes) && s.Runes[pos] == ' ' && (len(out.Runes) == 0 || out.Runes[len(out.Runes)-1] == ' ') {
			pos++
		}
	}
	out.Runes, out.Styles = append(out.Runes, s.Runes[pos:]...), append(out.Styles, s.Styles[pos:]...)
	for len(out.Runes) > 0 && unicode.IsSpace(out.Runes[0]) {
		out.Runes, out.Styles = out.Runes[1:], out.Styles[1:]
	}
	for n := len(out.Runes); n > 0 && unicode.IsSpace(out.Runes[n-1]); n-- {
		out.Runes, out.Styles = out.Runes[:n-1], out.Styles[:n-1]
	}
	return out, true
}

package bot

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/kit/txt"
	"github.com/robdevops/signalbot/internal/llm"
)

// Request is a message the bot has decided to answer.
type Request struct {
	Message chat.Message
	Chat    chat.ChatID
	Group   bool
	Key     string // history key: the chat id
	Prompt  string

	Kind    string // "mention", "reply" or "dm"
	Sender  string // display name and id, for the log
	Name    string // display name, else the id: short form for the log
	Display string // display name, "" if unknown
	Text    string // message text with the bot mention stripped

	Expires time.Duration // the chat's disappearing-messages timer, 0 if off
	Tool    string        // a chat command's tool, which the model must call
	Images  []chat.Media  // images in or quoted by the message
	Sticker *chat.Media   // a sticker sent as the message
}

// newRequest builds the prompt for an addressed message.
func newRequest(m chat.Message) *Request {
	text := m.Text
	if q := m.Quote; q != nil {
		// The sender composes the quote, so it may not be what the author
		// wrote. Escape it so it cannot close the wrapper and pose as a new
		// turn, and clamp it so it cannot flood. A message the sender tagged
		// the bot into is context they chose to share, whoever wrote it.
		quoted := strconv.Quote(txt.Clamp(q.Text, maxQuoteRunes))
		if q.FromBot {
			text = "[Replying to a message attributed to you: " + quoted + "]\n" + text
		} else {
			text = "[Replying to a message by " + strconv.Quote(cmp.Or(q.Author, "someone")) + ": " + quoted + "]\n" + text
		}
	}
	name := m.Sender.Name
	if m.Group && name != "" {
		text = name + ": " + text
	}
	who := m.Sender.ID
	if name != "" {
		who = name + "(" + m.Sender.ID + ")"
	}
	req := &Request{Message: m, Chat: m.Chat, Group: m.Group, Key: string(m.Chat), Prompt: text, Kind: m.Why, Sender: who,
		Name: cmp.Or(name, m.Sender.ID), Display: name, Text: m.Text, Expires: m.Expires}
	for _, md := range m.Media {
		switch {
		case md.Kind == chat.Sticker:
			req.Sticker = &md
		case len(req.Images) < llm.MaxImages:
			req.Images = append(req.Images, md)
		}
	}
	return req
}

// logLine says who sent the request, never what they said.
func (r *Request) logLine() string {
	line := r.Kind + " from " + r.Sender
	switch n := len(r.Images); n {
	case 0:
	case 1:
		line += " with 1 image"
	default:
		line += fmt.Sprintf(" with %d images", n)
	}
	if r.Sticker != nil {
		line += " with a sticker"
	}
	return line
}

// replyLogLine is the one-line summary of a model reply, e.g.
// "reply Alice 3.3s, tokens in/out: 1203/312 (10% cached), $0.002".
func replyLogLine(name string, took time.Duration, c llm.Completion) string {
	cached := 100 * min(c.CachedTokens, c.InputTokens) / max(c.InputTokens, 1)
	return fmt.Sprintf("reply %s %.1fs, tokens in/out: %d/%d (%d%% cached), $%.3f",
		name, took.Seconds(), c.InputTokens, c.OutputTokens, cached, c.Cost)
}

// parseCommand splits "/name rest" into the lowercased name and the rest.
func parseCommand(text string) (name, rest string, ok bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", "", false
	}
	name, rest, _ = strings.Cut(text[1:], " ")
	return strings.ToLower(name), strings.TrimSpace(rest), name != ""
}

// duplicate reports whether the same sender sent the same text to the same
// chat within the dedupe window, and records this one.
func (b *Bot) duplicate(req *Request) bool {
	key := req.Sender + "\x00" + req.Key + "\x00" + req.Text
	for _, a := range req.Images { // two different photos are not duplicates
		key += "\x00" + a.ID
	}
	if s := req.Sticker; s != nil {
		key += "\x00" + s.ID
	}
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, t := range b.dedupe {
		if now.Sub(t) > dedupeWindow {
			delete(b.dedupe, k)
		}
	}
	if _, ok := b.dedupe[key]; ok {
		return true
	}
	b.dedupe[key] = now
	return false
}

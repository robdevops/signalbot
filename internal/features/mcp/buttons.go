package mcp

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	ext "github.com/robdevops/signalbot/addon"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// An addon's direct answers with buttons are remembered, in memory only,
// for pressLife, the newest maxPressPosts: a press under one forgotten,
// such as after a restart, calls nothing.
const (
	maxPressPosts = 300
	pressLife     = 24 * time.Hour
)

// pressTimeout is how long a press's call may take, while the button
// spins; tests shorten it. maxToast caps the answer shown, in characters.
var pressTimeout = 5 * time.Second

const maxToast = 200

// pressPost is a direct answer of the addon's with buttons: the message
// they are under, and the tool their presses call; for a buttons op, the
// watch its result started, in the chat with pseudonym ChatID, the text it
// was sent with, if it went in one message, whether it had pictures, and
// when it was sent and how often edited since.
type pressPost struct {
	Chat    feature.Chat
	Sent    int64
	Tool    string
	Expires time.Time

	ChatID string
	Watch  string
	Text   string
	Single bool
	Pics   bool
	At     time.Time
	Edits  int
}

// markup is the buttons r asks for under its answer in chat, and the tool
// their presses call; none if r asks for none, if they break the rules,
// which is logged, or if chat's network shows no buttons.
func (a *addon) markup(log logx.L, chat feature.Chat, r toolResult) (feature.Markup, string) {
	btns, tool := r.Meta.Buttons, r.Meta.OnPress
	if len(btns) == 0 && tool == "" {
		return feature.Markup{}, ""
	}
	refuse := func(why string) (feature.Markup, string) {
		log.Warnf("MCP server %s: buttons left off: %s", a.server, why)
		return feature.Markup{}, ""
	}
	switch {
	case len(btns) == 0 || tool == "":
		return refuse("buttons without onPress, or onPress without buttons")
	case !a.only[tool]:
		return refuse("an onPress that isn't one of its tools for the bot alone")
	}
	mk, why := buttonRows(btns)
	if why != "" {
		return refuse(why)
	}
	if a.host == nil || !a.host.Net(chat).Caps.Buttons {
		return feature.Markup{}, ""
	}
	return mk, tool
}

// buttonRows is btns as markup, or why they break the rules.
func buttonRows(btns [][]ext.Button) (feature.Markup, string) {
	var mk feature.Markup
	if len(btns) == 0 || len(btns) > maxRows || slices.ContainsFunc(btns, func(row []ext.Button) bool { return len(row) == 0 || len(row) > maxPerRow }) {
		return mk, "more than 5 rows of 5 buttons, an empty row, or none"
	}
	for _, row := range btns {
		var out []feature.Button
		for _, b := range row {
			if !label(b.Label) || b.Data == "" || len(b.Data) > maxData {
				return feature.Markup{}, "a button's label or data empty or too long"
			}
			out = append(out, feature.Button{Label: b.Label, Data: b.Data})
		}
		mk.Buttons = append(mk.Buttons, out)
	}
	return mk, ""
}

// keepPress remembers p, the answer whose buttons are under p.Sent in
// p.Chat, in place of an earlier one in its chat for the same watch.
func (a *addon) keepPress(p pressPost) {
	now := a.now()
	p.ChatID, p.At, p.Expires = a.chatID(p.Chat), now, now.Add(pressLife)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.presses = slices.DeleteFunc(a.presses, func(x pressPost) bool {
		return !now.Before(x.Expires) || x.Chat == p.Chat && x.Sent == p.Sent || p.Watch != "" && x.ChatID == p.ChatID && x.Watch == p.Watch
	})
	a.presses = append(a.presses, p)
	if n := len(a.presses) - maxPressPosts; n > 0 {
		a.presses = slices.Delete(a.presses, 0, n)
	}
}

// rebutton does a buttons op: it replaces the buttons under the addon's
// remembered direct answer whose result started o.Watch in the chat
// o.Chat names, keeping its onPress, by an edit, within the network's edit
// limits; and its text too if o has text, filled in and clamped as a
// post's, which later buttons ops then keep. It returns why the op was dropped, if it was; one
// for an answer it doesn't remember, or can't edit now, is done with
// nothing changed.
func (a *addon) rebutton(o ext.Op) (why string) {
	mk, why := buttonRows(o.Buttons)
	if why != "" {
		return "buttons ops with " + why
	}
	now := a.now()
	a.mu.Lock()
	i := slices.IndexFunc(a.presses, func(x pressPost) bool {
		return o.Watch != "" && x.ChatID == o.Chat && x.Watch == o.Watch && now.Before(x.Expires)
	})
	var p pressPost
	if i >= 0 {
		p = a.presses[i]
	}
	a.mu.Unlock()
	if i < 0 || a.host == nil || !a.allows(p.Chat) {
		return ""
	}
	log, caps := a.logFor(p.Chat), a.host.Net(p.Chat).Caps
	switch {
	case p.Pics:
		return "buttons ops for an answer with pictures"
	case !caps.Buttons:
		return ""
	case !p.Single:
		log.Debugf("MCP server %s: buttons left as they are: the answer went in several messages", a.server)
		return ""
	case caps.EditLimit > 0 && p.Edits >= caps.EditLimit, caps.EditWindow > 0 && now.Sub(p.At) >= caps.EditWindow:
		log.Debugf("MCP server %s: buttons left as they are: past the network's edit limits", a.server)
		return ""
	}
	text := p.Text
	if t := strings.TrimSpace(o.Text); t != "" {
		text = txt.Clamp(a.fill(p.Chat, t), maxAnswer)
	}
	a.host.Post(feature.Post{Chat: p.Chat, Text: text, Edit: p.Sent, Markup: mk, OnSent: func(sent []int64, dropped bool) {
		if dropped || len(sent) == 0 {
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if j := slices.IndexFunc(a.presses, func(x pressPost) bool { return x.Chat == p.Chat && x.Sent == p.Sent }); j >= 0 {
			x := &a.presses[j]
			x.Text = text
			if last := sent[len(sent)-1]; last != x.Sent || len(sent) > 1 { // sent anew
				x.Sent, x.Single, x.At, x.Edits = last, len(sent) == 1, a.now(), 0
			} else {
				x.Edits++
			}
		}
	}})
	return ""
}

// press calls the onPress tool of the addon's answer whose buttons p was
// pressed under, with the button's data and the presser's context, and
// returns the result's text, shown to the presser; ok is false if the
// answer isn't one of the addon's it remembers. The press leases the chat,
// so the addon can post what it does. Only the call's outcome, time and
// size are logged.
func (a *addon) press(ctx context.Context, p feature.Press) (answer string, ok bool) {
	if p.Sent == 0 {
		return "", false
	}
	now := a.now()
	a.mu.Lock()
	i := slices.IndexFunc(a.presses, func(x pressPost) bool { return x.Sent == p.Sent && sameChat(x.Chat, p.Chat) && now.Before(x.Expires) })
	var post pressPost
	if i >= 0 {
		post = a.presses[i]
	}
	a.mu.Unlock()
	if i < 0 {
		return "", false
	}
	p.Chat = post.Chat // in a forum, its topic, which a press on an older message doesn't name
	log := logx.Ctx(ctx)
	switch {
	case !a.allows(p.Chat) || p.By.ID == "":
		return "", true
	case !a.only[post.Tool]:
		log.Warnf("MCP server %s: a press refused: its tool isn't one for the bot alone", a.server)
		return "", true
	}
	select {
	case a.busy <- struct{}{}:
		defer func() { <-a.busy }()
	default:
		log.Warnf("MCP server %s: a press refused: %d calls already in flight", a.server, maxDirect)
		return a.title + " is busy, try again in a moment.", true
	}
	a.lease(p.Chat)
	ctx, cancel := context.WithTimeout(ctx, pressTimeout)
	defer cancel()
	start := time.Now()
	cc := a.context(feature.Message{Chat: p.Chat, Sender: p.By.ID, Name: p.By.Name, Timestamp: p.Sent, Bot: true, Addressed: true}, now)
	cc.Deadline = start.Add(pressTimeout).Unix()
	res, err := a.conn.call(ctx, "tools/call", map[string]any{
		"name":      post.Tool,
		"arguments": ext.Press{Press: p.Data},
		"_meta":     map[string]any{ext.KeyContext: cc},
	})
	var r toolResult
	outcome := "ok"
	switch {
	case err != nil:
		outcome = errCode(err)
	case json.Unmarshal(res, &r) != nil:
		outcome = "a malformed result"
	case r.IsError:
		outcome = "an error result"
	}
	log.Infof("MCP server %s: a press called %s: %s in %dms, %d bytes", a.server, post.Tool, outcome, time.Since(start).Milliseconds(), len(res))
	if outcome != "ok" {
		return a.title + " couldn't do that just now.", true
	}
	var parts []string
	for _, c := range r.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		}
	}
	return txt.Clamp(a.fill(p.Chat, strings.TrimSpace(strings.Join(parts, "\n"))), maxToast-1), true
}

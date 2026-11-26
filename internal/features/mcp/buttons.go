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
// they are under, and the tool their presses call.
type pressPost struct {
	Chat    feature.Chat
	Sent    int64
	Tool    string
	Expires time.Time
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
	case len(btns) > maxRows || slices.ContainsFunc(btns, func(row []ext.Button) bool { return len(row) == 0 || len(row) > maxPerRow }):
		return refuse("more than 5 rows of 5 buttons, or an empty row")
	}
	var mk feature.Markup
	for _, row := range btns {
		var out []feature.Button
		for _, b := range row {
			if !label(b.Label) || b.Data == "" || len(b.Data) > maxData {
				return refuse("a button's label or data empty or too long")
			}
			out = append(out, feature.Button{Label: b.Label, Data: b.Data})
		}
		mk.Buttons = append(mk.Buttons, out)
	}
	if a.host == nil || !a.host.Net(chat).Caps.Buttons {
		return feature.Markup{}, ""
	}
	return mk, tool
}

// keepPress remembers that the buttons under sent, in chat, call tool.
func (a *addon) keepPress(chat feature.Chat, sent int64, tool string) {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	a.presses = slices.DeleteFunc(a.presses, func(x pressPost) bool {
		return !now.Before(x.Expires) || x.Chat == chat && x.Sent == sent
	})
	a.presses = append(a.presses, pressPost{Chat: chat, Sent: sent, Tool: tool, Expires: now.Add(pressLife)})
	if n := len(a.presses) - maxPressPosts; n > 0 {
		a.presses = slices.Delete(a.presses, 0, n)
	}
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

package mcp

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	ext "github.com/robdevops/signalbot/addon"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// A post's markup's limits, as addon/schema.json says: rows of buttons or
// keys, and keys or buttons in a row; a label's size in characters, a
// button's data's in bytes (Telegram's), and a key's command's in
// characters.
const (
	maxRows    = 5
	maxPerRow  = 5
	maxLabel   = 32
	maxData    = 64
	maxCommand = 256
)

// markup is what o carries besides its content, checked, or why it can't
// be posted. Buttons and a keyboard are left off where the network shows
// none, as is a keyboard in a group, by the network.
func (a *addon) markup(o ext.Op) (feature.Markup, string) {
	mk := feature.Markup{Silent: o.Silent, NoKeyboard: o.RemoveKeyboard}
	buttons, keys := len(o.Buttons) > 0, len(o.Keyboard) > 0
	switch {
	case buttons != (o.OnPress != ""):
		return mk, "buttons without onPress, or onPress without buttons"
	case buttons && !a.only[o.OnPress]:
		return mk, "an onPress that isn't one of its tools for the bot alone"
	case (buttons || keys || o.RemoveKeyboard) && len(o.Images) > 1:
		return mk, "buttons or a keyboard and more than one image"
	case buttons && (keys || o.RemoveKeyboard), keys && o.RemoveKeyboard:
		return mk, "buttons and a keyboard, or a keyboard and removeKeyboard"
	case !grid(len(o.Buttons), func(i int) int { return len(o.Buttons[i]) }) || !grid(len(o.Keyboard), func(i int) int { return len(o.Keyboard[i]) }):
		return mk, "more than 5 rows of 5 buttons or keys, or an empty row"
	}
	for _, row := range o.Buttons {
		var r []feature.Button
		for _, b := range row {
			if !label(b.Label) || b.Data == "" || len(b.Data) > maxData {
				return mk, "a button's label or data empty or too long"
			}
			r = append(r, feature.Button{Label: b.Label, Data: b.Data})
		}
		mk.Buttons = append(mk.Buttons, r)
	}
	var labels []string
	for _, row := range o.Keyboard {
		var r []feature.Key
		for _, k := range row {
			if !label(k.Label) || slices.Contains(labels, k.Label) || !strings.HasPrefix(k.Command, "/") ||
				utf8.RuneCountInString(k.Command) > maxCommand || strings.ContainsAny(k.Command, "\r\n") {
				return mk, "a key's label empty, too long or twice, or its command not a command"
			}
			if !a.cmds[strings.ToLower(strings.TrimPrefix(strings.Fields(k.Command)[0], "/"))] {
				return mk, "a key's command not one of its commands"
			}
			labels = append(labels, k.Label)
			r = append(r, feature.Key{Label: k.Label, Command: k.Command})
		}
		mk.Keyboard = append(mk.Keyboard, r)
	}
	if !a.buttons {
		mk.Buttons, mk.Keyboard, mk.NoKeyboard = nil, nil, false
	}
	return mk, ""
}

// grid reports whether rows rows, of size(i) each, are at most maxRows of
// 1 to maxPerRow.
func grid(rows int, size func(i int) int) bool {
	if rows > maxRows {
		return false
	}
	for i := range rows {
		if n := size(i); n == 0 || n > maxPerRow {
			return false
		}
	}
	return true
}

// label reports whether s is a button's or key's label: 1 to maxLabel
// characters, with no space around them, as Telegram may trim a key's.
func label(s string) bool {
	return s != "" && strings.TrimSpace(s) == s && utf8.RuneCountInString(s) <= maxLabel
}

// edit replaces the text, image and buttons of the addon's post o.Post in
// o.Chat, if the addon may, as for a post: with the post permission, in a
// chat it has a lease in, within the rate. The post must be one it
// remembers sending there. One edited again, after it was dropped, isn't
// counted against the rate again.
func (a *addon) edit(o ext.Op, again bool) (why string) {
	now := a.now()
	a.mu.Lock()
	p, known := a.posted[o.Post]
	l, leased := a.leases[o.Chat]
	a.mu.Unlock()
	switch {
	case !a.cfg.Post:
		return "edits not allowed"
	case o.Post == "" || len(o.Keyboard) > 0 || o.RemoveKeyboard || o.ReplyTo != "" || o.Audio != nil:
		return "malformed edits"
	case !known || p.Chat != o.Chat:
		return "edits of an unknown post"
	case a.uneditable(p, now) != "":
		return a.uneditable(p, now)
	case !leased || !l.live(now) || !a.allows(l.Chat):
		return "for a chat without a lease"
	case len(o.Images) > 1:
		return "edits with more than one image"
	}
	mk, why := a.markup(o)
	if why != "" {
		return "edits with " + why
	}
	mk.Silent = false // an edit notifies no one
	var b64 []string
	for _, im := range o.Images {
		b64 = append(b64, im.Data)
	}
	out := &outgoing{Post: feature.Post{Chat: l.Chat, Text: txt.Clamp(a.fill(l.Chat, strings.TrimSpace(o.Text)), maxAnswer), Pictures: a.pictures(b64), Markup: mk, Edit: p.last()},
		ops: []string{o.ID}, edits: o.Post, onPress: o.OnPress}
	if len(mk.Buttons) == 0 {
		out.onPress = ""
	}
	switch {
	case out.Text == "" && len(out.Pictures) == 0:
		return "empty edits"
	case !again && !a.posts.allow(l.Chat.Key(), now):
		return "edits over the rate limit"
	}
	a.send(o.Chat, out)
	return ""
}

// pressTimeout is how long a press's call may take, while the button
// spins; tests shorten it. maxToast caps the answer shown, in characters.
var pressTimeout = 10 * time.Second

const maxToast = 200

// press calls the onPress tool of the addon's post whose buttons p was
// pressed under, with the button's data, the post op's ID and the
// presser's context, and returns the result's text, shown to the presser;
// ok is false if the post isn't one of the addon's it remembers. The press
// leases the chat, so the addon can answer it, such as by editing the
// post, however old the post is. Only the call's outcome, time and size
// are logged.
func (a *addon) press(ctx context.Context, p feature.Press) (answer string, ok bool) {
	if !a.allows(p.Chat) || p.By.ID == "" || p.Sent == 0 {
		return "", false
	}
	chat, now := a.chatID(p.Chat), a.now()
	bare := p.Chat.GroupID != "" && !strings.Contains(p.Chat.GroupID, "/")
	op, tool := "", ""
	a.mu.Lock()
	for id, x := range a.posted {
		if x.OnPress == "" || x.last() != p.Sent || !now.Before(x.Expires) {
			continue
		}
		if x.Chat == chat {
			op, tool = id, x.OnPress
			break
		}
		if bare && x.Topic != nil && strings.HasPrefix(x.Topic.GroupID, p.Chat.GroupID+"/") { // under an old message, which names no topic
			op, tool, p.Chat = id, x.OnPress, *x.Topic
			break
		}
	}
	a.mu.Unlock()
	switch {
	case op == "" || !a.allows(p.Chat):
		return "", false
	case !a.only[tool]:
		logx.Warnf("MCP server %s: a press refused: its tool isn't one for the bot alone", a.server)
		return "", true
	}
	select {
	case a.busy <- struct{}{}:
		defer func() { <-a.busy }()
	default:
		logx.Warnf("MCP server %s: a press refused: %d calls already in flight", a.server, maxDirect)
		return a.title + " is busy, try again in a moment.", true
	}
	a.lease(p.Chat, false)
	ctx, cancel := context.WithTimeout(ctx, pressTimeout)
	defer cancel()
	start := time.Now()
	cc := a.context(feature.Message{Chat: p.Chat, Sender: p.By.ID, Name: p.By.Name, Timestamp: p.Sent, Bot: true, Addressed: true}, now)
	cc.Deadline = start.Add(pressTimeout).Unix()
	res, err := a.conn.call(ctx, "tools/call", map[string]any{
		"name":      tool,
		"arguments": ext.Press{Press: p.Data, Post: op},
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
	logx.Infof("MCP server %s: a press called %s: %s in %dms, %d bytes", a.server, tool, outcome, time.Since(start).Milliseconds(), len(res))
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

package mcp

import (
	"context"
	"maps"
	"slices"
	"strings"
	"sync"
	"unicode/utf16"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

// maxReruns is how many direct commands' posts are kept for their buttons,
// the oldest forgotten first: a button under one forgotten does nothing.
const maxReruns = 300

// Button data under a re-runnable post: refresh, or "=" and a value.
const (
	pressRefresh = "rerun"
	pressValue   = "rerun="
)

// reruns are the posts of plain servers' direct commands, and feed items'
// charts, each with the call it shows, so a button under one makes the
// call again, with another of its values or the same, and edits the post.
// Only on a network with buttons; where there are none, /name value does
// it.
type reruns struct {
	store  feature.Store
	h      feature.Host
	cmds   map[string]rerunCmd // by server and command, "server/name"
	charts map[string]drawer   // by server, those with a feed

	mu    sync.Mutex
	posts []rerunPost // oldest first
}

// rerunCmd is a command a post can be made again by.
type rerunCmd struct {
	d *plainDirect
	c chatCommand
}

// drawer draws a chart: tool called with args, its pictures; none if it
// can't.
type drawer func(ctx context.Context, tool string, args map[string]any) []feature.Picture

// rerunPost is a post and the call it shows, as saved: a direct command,
// or a feed item's chart (Tool set), whose text stays as the item's.
type rerunPost struct {
	Chat    feature.Chat `json:"chat"`
	Sent    int64        `json:"sent"` // the message its buttons are under
	Server  string       `json:"server"`
	Command string       `json:"command,omitempty"`
	Value   string       `json:"value,omitempty"`   // its arg's value, if any
	Picture bool         `json:"picture,omitempty"` // it is a picture, its text the caption

	Tool      string         `json:"tool,omitempty"`
	Arguments map[string]any `json:"arguments,omitempty"` // with Arg's value as it is now
	Arg       string         `json:"arg,omitempty"`
	Values    []string       `json:"values,omitempty"`
	Text      string         `json:"text,omitempty"`
}

// newReruns are the posts saved in store, if it can be read.
func newReruns(h feature.Host, store feature.Store) *reruns {
	r := &reruns{store: store, h: h, cmds: map[string]rerunCmd{}, charts: map[string]drawer{}}
	if _, err := store.Load(&r.posts); err != nil {
		logx.Warnf("MCP: the direct commands' posts can't be read, so buttons under them do nothing: %v", err)
		r.posts = nil
	}
	return r
}

// add lets name, a command of d, be made again from its posts.
func (r *reruns) add(d *plainDirect, name string, c chatCommand) {
	r.cmds[d.server+"/"+name] = rerunCmd{d, c}
}

// offer gives act, the answer to /name with value, buttons, and remembers
// the post it goes out in.
// Only an answer that goes out as one message gets them, so the buttons
// are on it and an edit can replace all of it.
func (r *reruns) offer(act *feature.Action, chat feature.Chat, d *plainDirect, name, value string) {
	if act.Answer == "" && len(act.Pictures) == 0 || !r.h.Net(chat).Caps.Buttons || !r.oneMessage(*act, chat) {
		return
	}
	act.Markup.Buttons = cmdButtons(r.cmds[d.server+"/"+name].c, value)
	pic := len(act.Pictures) > 0
	act.OnSent = func(sent []int64, dropped bool) {
		if !dropped && len(sent) == 1 {
			r.keep(rerunPost{Chat: chat, Sent: sent[0], Server: d.server, Command: name, Value: value, Picture: pic}, 0)
		}
	}
}

// oneMessage reports whether act goes out as one message: text alone, or
// one picture with its text as the caption, where the network limits one.
// Markdown can only shorten the text, so a fit counted before it is one.
func (r *reruns) oneMessage(act feature.Action, chat feature.Chat) bool {
	limit := r.h.Net(chat).Caps.Caption
	switch len(act.Pictures) {
	case 0:
		return true
	case 1:
		return limit == 0 || len(utf16.Encode([]rune(act.Answer))) <= limit
	}
	return false
}

// cmdButtons are the buttons of c's post with value ("" for its default).
func cmdButtons(c chatCommand, value string) [][]feature.Button {
	if value == "" {
		value, _ = c.Args[c.Arg].(string)
	}
	return buttons(c.Values, value)
}

// chart offers the buttons of a feed item's chart, drawn by server's tool
// with args, Arg's values the choices, under the post of text and pics in
// chat, if it is one message; and remembers the post.
func (r *reruns) chart(p *feature.Post, server, tool string, args map[string]any, arg string, values []string) {
	if len(p.Pictures) != 1 || !r.h.Net(p.Chat).Caps.Buttons || !r.oneMessage(feature.Action{Answer: p.Text, Pictures: p.Pictures}, p.Chat) {
		return
	}
	current, _ := args[arg].(string)
	if !slices.Contains(values, current) {
		values = nil // no arg, or its value isn't one of them: refresh alone
	}
	p.Markup.Buttons = buttons(values, current)
	post := rerunPost{Chat: p.Chat, Server: server, Picture: true, Tool: tool, Arguments: args, Arg: arg, Values: values, Text: p.Text}
	p.OnSent = func(sent []int64, dropped bool) {
		if !dropped && len(sent) == 1 {
			post.Sent = sent[0]
			r.keep(post, 0)
		}
	}
}

// buttons are a post's buttons: one per value, the current one
// highlighted (Telegram: blue), if they fit, and refresh.
func buttons(values []string, value string) [][]feature.Button {
	fit := len(values) < maxRows*maxPerRow && !slices.ContainsFunc(values, func(v string) bool { return len(pressValue)+len(v) > maxData })
	var btns []feature.Button
	if fit {
		for _, v := range values {
			btns = append(btns, feature.Button{Label: clampLabel(v), Data: pressValue + v, Highlight: v == value})
		}
	}
	btns = append(btns, feature.Button{Label: "🔄", Data: pressRefresh})
	return slices.Collect(slices.Chunk(btns, maxPerRow))
}

// clampLabel is s cut to a button label's size.
func clampLabel(s string) string {
	if r := []rune(s); len(r) > maxLabel {
		return string(r[:maxLabel])
	}
	return s
}

// sameChat reports whether a post in chat could be pressed as in pressed:
// the same chat, or, on Telegram, a forum topic of the group a press under
// an older message names alone. Telegram's message IDs are the group's,
// whatever the topic.
func sameChat(chat, pressed feature.Chat) bool {
	return chat == pressed || pressed.GroupID != "" && !strings.Contains(pressed.GroupID, "/") && strings.HasPrefix(chat.GroupID, pressed.GroupID+"/")
}

// keep remembers p, in place of the post sent as old if that isn't 0, and
// saves them all.
func (r *reruns) keep(p rerunPost, old int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.posts = slices.DeleteFunc(r.posts, func(x rerunPost) bool {
		return x.Chat == p.Chat && (x.Sent == p.Sent || old != 0 && x.Sent == old)
	})
	r.posts = append(r.posts, p)
	if n := len(r.posts) - maxReruns; n > 0 {
		r.posts = slices.Delete(r.posts, 0, n)
	}
	if err := r.store.Save(r.posts); err != nil {
		logx.Warnf("MCP: the direct commands' posts can't be saved: %v", err)
	}
}

// press makes a post's call again, if p is under one, and edits the post
// with what comes back. It runs in the chat's slot, so presses in a chat
// take turns, and their edits go out in order.
func (r *reruns) press(ctx context.Context, p feature.Press) (answer string, ok bool) {
	value, chose := strings.CutPrefix(p.Data, pressValue)
	if !chose && p.Data != pressRefresh {
		return "", false
	}
	r.mu.Lock()
	i := slices.IndexFunc(r.posts, func(x rerunPost) bool { return x.Sent == p.Sent && sameChat(x.Chat, p.Chat) })
	var post rerunPost
	if i >= 0 {
		post = r.posts[i]
	}
	r.mu.Unlock()
	if i < 0 {
		return "", false // an addon's, or forgotten
	}
	p.Chat = post.Chat // in a forum, its topic, which a press on an older message doesn't name
	if post.Tool != "" {
		return r.redraw(ctx, p, post, value, chose)
	}
	cmd, known := r.cmds[post.Server+"/"+post.Command]
	switch {
	case !known:
		return "That button no longer works.", true
	case !chose:
		value = post.Value
	case !slices.Contains(cmd.c.Values, value):
		return "That choice no longer works.", true
	}
	act, done := cmd.d.call(ctx, post.Command, cmd.c, value)
	if !done {
		return act.Answer, true // told on the button, leaving the post as it was
	}
	next := post
	next.Value, next.Picture = value, len(act.Pictures) > 0
	switch {
	case !r.oneMessage(act, p.Chat):
		// Too much for one message: sent anew, without buttons, and the
		// post stays as it was, its buttons working.
		r.h.Post(feature.Post{Chat: p.Chat, Text: act.Answer, Pictures: act.Pictures})
	case next.Picture != post.Picture:
		// A picture where there was none, or none where there was one: a
		// message can't become the other, so it is a new post, with its own
		// buttons, and the old one keeps its own.
		r.h.Post(feature.Post{Chat: p.Chat, Text: act.Answer, Pictures: act.Pictures,
			Markup: feature.Markup{Buttons: cmdButtons(cmd.c, value)},
			OnSent: func(sent []int64, dropped bool) {
				if !dropped && len(sent) == 1 {
					next.Sent = sent[0]
					r.keep(next, 0)
				}
			}})
	default:
		r.h.Post(feature.Post{Chat: p.Chat, Text: act.Answer, Pictures: act.Pictures, Edit: p.Sent,
			Markup: feature.Markup{Buttons: cmdButtons(cmd.c, value)},
			OnSent: func(sent []int64, dropped bool) {
				if len(sent) == 1 {
					next.Sent = sent[0]
					r.keep(next, p.Sent)
				}
			}})
	}
	return "", true
}

// redraw makes a feed item's chart again, with value as its arg if chose,
// else as it was, and edits the post: the item's text over the new chart.
func (r *reruns) redraw(ctx context.Context, p feature.Press, post rerunPost, value string, chose bool) (string, bool) {
	draw, known := r.charts[post.Server]
	switch {
	case !known:
		return "That button no longer works.", true
	case chose && !slices.Contains(post.Values, value):
		return "That choice no longer works.", true
	}
	args := maps.Clone(post.Arguments)
	if args == nil {
		args = map[string]any{}
	}
	if chose {
		args[post.Arg] = value
	}
	pics := draw(ctx, post.Tool, args)
	if len(pics) == 0 {
		return "The chart couldn't be drawn just now.", true
	}
	next := post
	next.Arguments = args
	current, _ := args[post.Arg].(string)
	r.h.Post(feature.Post{Chat: p.Chat, Text: post.Text, Pictures: pics[:1], Edit: p.Sent,
		Markup: feature.Markup{Buttons: buttons(post.Values, current)},
		OnSent: func(sent []int64, dropped bool) {
			if len(sent) == 1 {
				next.Sent = sent[0]
				r.keep(next, p.Sent)
			}
		}})
	return "", true
}

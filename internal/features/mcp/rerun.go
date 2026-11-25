package mcp

import (
	"context"
	"slices"
	"strings"
	"sync"

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

// reruns are the posts of plain servers' direct commands, each with the
// call it answered, so a button under one makes the call again, with
// another of the command's values or the same, and edits the post. Only on
// a network with buttons; where there are none, /name value does it.
type reruns struct {
	store feature.Store
	h     feature.Host
	cmds  map[string]rerunCmd // by server and command, "server/name"

	mu    sync.Mutex
	posts []rerunPost // oldest first
}

// rerunCmd is a command a post can be made again by.
type rerunCmd struct {
	d *plainDirect
	c chatCommand
}

// rerunPost is a post and the call it shows, as saved.
type rerunPost struct {
	Chat    feature.Chat `json:"chat"`
	Sent    int64        `json:"sent"` // the message its buttons are under
	Server  string       `json:"server"`
	Command string       `json:"command"`
	Value   string       `json:"value,omitempty"` // its arg's value, if any
}

// newReruns are the posts saved in store, if it can be read.
func newReruns(h feature.Host, store feature.Store) *reruns {
	r := &reruns{store: store, h: h, cmds: map[string]rerunCmd{}}
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
func (r *reruns) offer(act *feature.Action, chat feature.Chat, d *plainDirect, name, value string) {
	if act.Answer == "" && len(act.Pictures) == 0 {
		return
	}
	act.Markup.Buttons = buttons(r.cmds[d.server+"/"+name].c, value)
	act.OnSent = func(sent []int64, dropped bool) {
		if !dropped && len(sent) > 0 {
			r.keep(rerunPost{Chat: chat, Sent: sent[len(sent)-1], Server: d.server, Command: name, Value: value}, 0)
		}
	}
}

// buttons are a post's buttons, for c with value: one per value, the
// current one marked, if they fit, and refresh.
func buttons(c chatCommand, value string) [][]feature.Button {
	var row []feature.Button
	var rows [][]feature.Button
	fit := len(c.Values) < maxRows*maxPerRow
	for _, v := range c.Values {
		if len(pressValue)+len(v) > maxData {
			fit = false
		}
	}
	if fit {
		for _, v := range c.Values {
			label := v
			if v == value {
				label = "• " + v
			}
			if len(row) == maxPerRow {
				rows, row = append(rows, row), nil
			}
			row = append(row, feature.Button{Label: clampLabel(label), Data: pressValue + v})
		}
	}
	if len(row) == maxPerRow {
		rows, row = append(rows, row), nil
	}
	return append(rows, append(row, feature.Button{Label: "🔄", Data: pressRefresh}))
}

// clampLabel is s cut to a button label's size.
func clampLabel(s string) string {
	if r := []rune(s); len(r) > maxLabel {
		return string(r[:maxLabel])
	}
	return s
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
	i := slices.IndexFunc(r.posts, func(x rerunPost) bool { return x.Chat == p.Chat && x.Sent == p.Sent })
	var post rerunPost
	if i >= 0 {
		post = r.posts[i]
	}
	r.mu.Unlock()
	if i < 0 {
		return "", false // an addon's, or forgotten
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
	next.Value = value
	r.h.Post(feature.Post{Chat: p.Chat, Text: act.Answer, Pictures: act.Pictures, Edit: p.Sent,
		Markup: feature.Markup{Buttons: buttons(cmd.c, value)},
		OnSent: func(sent []int64, dropped bool) {
			if len(sent) > 0 {
				next.Sent = sent[len(sent)-1]
				r.keep(next, p.Sent)
			}
		}})
	return "", true
}

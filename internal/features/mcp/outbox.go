package mcp

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// outboxURI is the resource an addon queues its posts and other ops in.
// The bot subscribes to it, and reads outboxURI/<cursor> for the ops past
// cursor.
const outboxURI = "signalbot://outbox"

// maxPages caps the pages of one outbox read, so an addon that always has
// more can't keep the reader.
const maxPages = 100

// Reading the outbox again: after a failed read, from retryDelay, doubling
// to maxRetryDelay while reads keep failing; a post dropped before it was
// sent is posted again after retryDelay, doubling, and given up after
// maxPostTries. Tests shorten retryDelay.
var retryDelay = 10 * time.Second

const (
	maxRetryDelay = 5 * time.Minute
	maxPostTries  = 6
)

// ready subscribes to the addon's outbox, once it has started (again), if
// it takes subscriptions, then reads the outbox, for anything sent while
// the bot wasn't listening.
func (a *addon) ready(ctx context.Context, hs handshake) {
	select {
	case a.up <- struct{}{}:
	default:
	}
	if !hs.subscribe {
		logx.Warnf("MCP server %s takes no resource subscriptions, so its outbox is off", a.server)
		return
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if _, err := a.conn.call(ctx, "resources/subscribe", map[string]string{"uri": outboxURI}); err != nil {
		logx.Warnf("MCP server %s: subscribing to its outbox failed: %s", a.server, errCode(err))
		return
	}
	a.poke()
}

// poke marks the outbox as having news, without waiting.
func (a *addon) poke() {
	select {
	case a.dirty <- struct{}{}:
	default:
	}
}

// readOutbox reads the outbox each time it has news, and again a while
// after a read fails, until ctx ends. It is the addon's one reader, so two
// reads never race, and news that comes during a read is read after it.
func (a *addon) readOutbox(ctx context.Context) {
	wait := retryDelay
	var again <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.dirty:
		case <-again:
		}
		if a.drain(ctx) {
			wait, again = retryDelay, nil
		} else {
			again = time.After(wait)
			wait = min(wait*2, maxRetryDelay)
		}
	}
}

// page is a page of an addon's outbox.
type page struct {
	Cursor string `json:"cursor"`
	More   bool   `json:"more"`
	Ops    []op   `json:"ops"`
}

// op is one thing an addon asks the bot to do.
type op struct {
	ID     string      `json:"id"`
	Op     string      `json:"op"`   // post, react or unwatch
	Chat   string      `json:"chat"` // a post's or unwatch's chat pseudonym
	Text   string      `json:"text"`
	Images []postImage `json:"images"`
	Ref    string      `json:"ref"` // a reaction's message
	Emoji  string      `json:"emoji"`
	Watch  string      `json:"watch"` // the ID of the watch to end
}

// postImage is an image in a post.
type postImage struct {
	Data     string `json:"data"` // base64
	MimeType string `json:"mimeType"`
	Name     string `json:"name"`
}

// errNotFound is a resource the server doesn't have: -32002 in the
// 2025-06-18 spec and older SDKs, -32602 in the Go SDK.
func errNotFound(err error) bool {
	re, ok := errors.AsType[*rpcError](err)
	return ok && (re.Code == -32002 || re.Code == -32602)
}

// drain reads the outbox page by page from the cursor until it has no
// more, applying each op not applied before, and reports whether it read
// it all. What is dropped is counted in one warning.
//
// Delivery is at least once: the cursor, which lets the addon forget the
// ops before it, moves past a post only once the post is sent. Until then
// a read starts from before it, and skips it while it waits; one dropped
// before it was sent, such as while the bot stops or before the network is
// up, is posted again by a later read. A page with more after it waits for
// its posts, since reading the next lets the addon forget them.
func (a *addon) drain(ctx context.Context) bool {
	dropped := map[string]int{}
	defer func() {
		if len(dropped) == 0 {
			return
		}
		var parts []string
		n := 0
		for _, why := range slices.Sorted(maps.Keys(dropped)) {
			parts = append(parts, fmt.Sprintf("%d %s", dropped[why], why))
			n += dropped[why]
		}
		unit := "ops"
		if n == 1 {
			unit = "op"
		}
		logx.Warnf("addon %s: dropped %d %s: %s", a.server, n, unit, strings.Join(parts, ", "))
	}()
	a.mu.Lock()
	a.next = "" // only this read's end may become the cursor
	a.mu.Unlock()
	for range maxPages {
		a.mu.Lock()
		cursor := cmp.Or(a.cursor, "0")
		a.mu.Unlock()
		rctx, cancel := context.WithTimeout(ctx, callTimeout)
		res, err := a.conn.call(rctx, "resources/read", map[string]string{"uri": outboxURI + "/" + cursor})
		cancel()
		var r struct {
			Contents []struct {
				Text string `json:"text"`
			} `json:"contents"`
		}
		var p page
		switch {
		case errNotFound(err):
			logx.Warnf("MCP server %s has no outbox to read", a.server)
			return true
		case err != nil:
			logx.Warnf("MCP server %s: reading its outbox failed: %s", a.server, errCode(err))
			return false
		case json.Unmarshal(res, &r) != nil || len(r.Contents) != 1 || json.Unmarshal([]byte(r.Contents[0].Text), &p) != nil:
			logx.Warnf("MCP server %s: its outbox was malformed (%d bytes)", a.server, len(res))
			return false
		}
		a.mu.Lock()
		a.missed = false // what was missed is read again now
		a.mu.Unlock()
		for _, o := range p.Ops {
			if why := a.apply(o); why != "" {
				dropped[why]++
			}
		}
		a.mu.Lock()
		a.next = cmp.Or(p.Cursor, cursor)
		a.settle()
		a.mu.Unlock()
		a.changed()
		if !p.More {
			return true
		}
		if !a.wait(ctx) {
			return false
		}
	}
	return false
}

// wait waits until every post read from the outbox has been sent or
// dropped, and reports whether the cursor then moved past them.
func (a *addon) wait(ctx context.Context) bool {
	for {
		a.mu.Lock()
		n, moved := len(a.sending), a.next == ""
		a.mu.Unlock()
		if n == 0 {
			return moved
		}
		select {
		case <-ctx.Done():
			return false
		case <-a.settled:
		}
	}
}

// settle moves the cursor to where the last read ended, once every post it
// read has been sent and none was missed. a.mu is held.
func (a *addon) settle() {
	if len(a.sending) == 0 && !a.missed && a.next != "" {
		a.cursor, a.next = a.next, ""
	}
}

// failure is a post op dropped before it was sent: how often, and when it
// may be posted again.
type failure struct {
	tries int
	retry time.Time
}

// apply does what o asks, once, and returns why it was dropped, if it was.
// A post counts as applied only once it is sent.
func (a *addon) apply(o op) (why string) {
	if o.ID == "" {
		return "without an id"
	}
	now := a.now()
	a.mu.Lock()
	f, failed := a.failed[o.ID]
	again := a.done[o.ID] || a.sending[o.ID]
	switch {
	case again:
	case o.Op == "post" && failed && now.Before(f.retry):
		again, a.missed = true, true // not yet: a later read posts it
	case o.Op != "post":
		a.remember(o.ID)
	}
	a.mu.Unlock()
	if again {
		return ""
	}
	switch o.Op {
	case "post":
		why = a.post(o, failed)
		if why != "" {
			a.mu.Lock()
			a.remember(o.ID)
			delete(a.failed, o.ID)
			a.mu.Unlock()
		}
		return why
	case "react":
		if !a.cfg.React {
			return "reactions not allowed"
		}
		a.mu.Lock()
		r, ok := a.refs[o.Ref]
		l, leased := a.leases[a.chatID(r.msg.Chat)]
		a.mu.Unlock()
		switch {
		case !ok || !now.Before(r.expires):
			return "reactions to an unknown message"
		case !leased || !now.Before(l.Expires) || !a.allows(r.msg.Chat):
			return "for a chat without a lease"
		}
		if why := a.react(r.msg, o.Emoji); why != "" {
			return "reactions " + why
		}
	case "unwatch":
		a.unwatch(o.Chat, o.Watch)
	default:
		return "of an unknown kind"
	}
	return ""
}

// post posts o in its chat, if the addon may. One posted again, after it
// was dropped, isn't counted against the rate again.
func (a *addon) post(o op, again bool) (why string) {
	now := a.now()
	a.mu.Lock()
	l, ok := a.leases[o.Chat]
	a.mu.Unlock()
	switch {
	case !a.cfg.Post:
		return "posts not allowed"
	case !ok || !now.Before(l.Expires) || !a.allows(l.Chat):
		return "for a chat without a lease"
	}
	var b64 []string
	for _, im := range o.Images {
		b64 = append(b64, im.Data)
	}
	out := &outgoing{Post: feature.Post{Chat: l.Chat, Label: a.title, Text: txt.Clamp(a.fill(l.Chat, strings.TrimSpace(o.Text)), maxAnswer), Pictures: a.pictures(b64)},
		ops: []string{o.ID}}
	switch {
	case out.Text == "" && len(out.Pictures) == 0:
		return "empty posts"
	case !again && !a.posts.allow(l.Chat.Key(), now):
		return "posts over the rate limit"
	}
	a.send(o.Chat, out)
	return ""
}

// outgoing is a post of the addon's, and the ops it carries.
type outgoing struct {
	feature.Post
	ops []string
}

// fits reports whether p's pictures can join o's in one post.
func (o *outgoing) fits(p *outgoing) bool {
	size := 0
	for _, pic := range append(slices.Clone(o.Pictures), p.Pictures...) {
		size += len(pic.Data)
	}
	return len(o.Pictures)+len(p.Pictures) <= maxAddonPictures && size <= maxPictureBytes
}

// send posts out in the chat whose pseudonym is id, unless a post of the
// addon's still waits in that chat's mailbox: then out is held, merged with
// the last one held if their pictures fit in one post, and posted once
// those before it have gone. So an addon never takes more than one place in
// a chat's mailbox.
func (a *addon) send(id string, out *outgoing) {
	a.mu.Lock()
	for _, op := range out.ops {
		a.sending[op] = true
	}
	if held, waiting := a.waiting[id]; waiting {
		if n := len(held); n > 0 && held[n-1].fits(out) {
			last := held[n-1]
			last.Text = txt.Clamp(strings.TrimSpace(last.Text+"\n\n"+out.Text), maxAnswer)
			last.Pictures = append(last.Pictures, out.Pictures...)
			last.ops = append(last.ops, out.ops...)
		} else {
			a.waiting[id] = append(held, out)
		}
		a.mu.Unlock()
		return
	}
	a.waiting[id] = nil
	a.mu.Unlock()
	a.mail(id, out)
}

// mail puts out in its chat's mailbox.
func (a *addon) mail(id string, out *outgoing) {
	p := out.Post
	p.Done = func(_ []int64, dropped bool) {
		a.sent(out.ops, !dropped)
		a.mu.Lock()
		held := a.waiting[id]
		if len(held) == 0 {
			delete(a.waiting, id)
		} else {
			a.waiting[id] = held[1:]
		}
		a.mu.Unlock()
		if len(held) > 0 {
			a.mail(id, held[0])
		}
	}
	a.host.Post(p)
}

// sent records that the post carrying ops was sent, or dropped before it
// was: a dropped one is read and posted again later, until it has been
// tried maxPostTries times.
func (a *addon) sent(ops []string, ok bool) {
	now := a.now()
	given, retries := 0, map[time.Duration]bool{}
	a.mu.Lock()
	for _, id := range ops {
		delete(a.sending, id)
		f := a.failed[id]
		switch {
		case ok:
			a.remember(id)
			delete(a.failed, id)
		case f.tries+1 >= maxPostTries:
			a.remember(id)
			delete(a.failed, id)
			given++
		default:
			retry := min(retryDelay<<f.tries, maxRetryDelay)
			a.failed[id] = failure{f.tries + 1, now.Add(retry)}
			a.missed, retries[retry] = true, true
		}
	}
	a.settle()
	settled := len(a.sending) == 0
	a.mu.Unlock()
	a.changed()
	if given > 0 {
		logx.Warnf("addon %s: %d posts dropped: not sent after %d tries", a.server, given, maxPostTries)
	}
	for retry := range retries {
		time.AfterFunc(retry, a.poke)
	}
	if settled {
		select {
		case a.settled <- struct{}{}:
		default:
		}
	}
}

// unwatch ends the watch called id in the chat whose pseudonym is chat;
// if it hasn't begun, it is ended before it does, for tombLife.
func (a *addon) unwatch(chat, id string) {
	a.mu.Lock()
	found := false
	for key, ws := range a.watches {
		a.watches[key] = slices.DeleteFunc(ws, func(w *watch) bool {
			gone := w.Chat == chat && w.ID == id
			found = found || gone
			return gone
		})
	}
	if !found {
		a.tombs[chat+"\x00"+id] = a.now().Add(tombLife)
	}
	a.mu.Unlock()
	a.changed()
}

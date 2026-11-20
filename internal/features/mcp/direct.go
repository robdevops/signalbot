package mcp

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/img"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

const (
	// maxDirect is how many direct calls may be in flight on one server; a
	// command past them is told the addon is busy, so a slow addon can't
	// take every slot.
	maxDirect = 2
	// maxAnswer caps a direct command's answer, in runes.
	maxAnswer = 4000
	// An addon's answer or post has at most maxAddonPictures pictures, of
	// at most maxPictureBytes in all.
	maxAddonPictures = 2
	maxPictureBytes  = 2 << 20
)

// directTimeout is how long a direct call may take, unless its command sets
// a timeout, and a watch's call; short of callTimeout so a hung addon can't
// hold a message's slot long; tests shorten it. maxDirectTimeout is the
// longest a command may set.
var directTimeout = 15 * time.Second

const maxDirectTimeout = 2 * time.Minute

// direct answers /name in m by calling the addon's tool c.Tool itself, with
// args and m's context, for at most c's timeout: the result's text and
// pictures, sent without the model. The context says when the bot stops
// waiting, so the addon can give up in time rather than act on a call
// nobody will see; one that runs past it is cancelled.
// A failure is told in words for people; the server's own text is never
// shown or logged.
func (a *addon) direct(ctx context.Context, m feature.Message, name string, c chatCommand, args string) feature.Action {
	if !a.allows(m.Chat) {
		return feature.Action{Answer: "/" + name + " isn't available in this chat."}
	}
	select {
	case a.busy <- struct{}{}:
		defer func() { <-a.busy }()
	default:
		logx.Warnf("MCP server %s: /%s refused: %d calls already in flight", a.server, name, maxDirect)
		return feature.Action{Answer: a.title + " is busy, try again in a moment."}
	}
	a.lease(m.Chat)
	timeout := cmp.Or(c.timeout, directTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	cc := a.context(m, a.now())
	cc.Deadline = start.Add(timeout).Unix() // rounded down, so never late
	res, err := a.conn.call(ctx, "tools/call", map[string]any{
		"name":      c.Tool,
		"arguments": map[string]string{"args": args},
		"_meta":     map[string]any{vendor + "context": cc},
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
	logx.Infof("MCP server %s: /%s called %s: %s in %dms, %d bytes", a.server, name, c.Tool, outcome, time.Since(start).Milliseconds(), len(res))
	if outcome != "ok" {
		return feature.Action{Answer: a.title + " couldn't do that just now."}
	}
	act := a.action(m.Chat, r)
	if r.Meta.Watch != nil {
		if w := a.watch(m, *r.Meta.Watch); w != nil && len(act.Pictures) > 0 {
			act.OnSent = func(sent []int64, _ bool) { a.guarding(w, sent) }
		}
	}
	if act.IsZero() {
		act.Done = true // nothing to say: still the addon's, not the model's
	}
	return act
}

// action is what a direct call's result says in chat: its text, with
// placeholders filled in, and its images made safe.
func (a *addon) action(chat feature.Chat, r toolResult) feature.Action {
	var parts, images []string
	for _, p := range r.Content {
		switch p.Type {
		case "text":
			parts = append(parts, p.Text)
		case "image":
			images = append(images, p.Data)
		}
	}
	text := strings.TrimSpace(strings.Join(parts, "\n"))
	return feature.Action{Answer: txt.Clamp(a.fill(chat, text), maxAnswer), Pictures: a.pictures(images)}
}

// pictures are images, in base64, made safe: at most maxAddonPictures, of
// at most maxPictureBytes in all. Those left out are counted in the log.
func (a *addon) pictures(images []string) []feature.Picture {
	var pics []feature.Picture
	total, left := 0, 0
	for _, b64 := range images {
		data, err := base64.StdEncoding.DecodeString(b64)
		if err == nil {
			data, err = img.Safe(data)
		}
		if err != nil || len(pics) == maxAddonPictures || total+len(data) > maxPictureBytes {
			left++
			continue
		}
		total += len(data)
		pics = append(pics, feature.Picture{Data: data})
	}
	if left > 0 {
		logx.Warnf("MCP server %s: %d images left out: not images, or past %d images or %d MB", a.server, left, maxAddonPictures, maxPictureBytes>>20)
	}
	return pics
}

// errCode says what went wrong with a call for the log: an error's code,
// never a server's text.
func errCode(err error) string {
	if re, ok := errors.AsType[*rpcError](err); ok {
		return fmt.Sprintf("rpc error %d", re.Code)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "no answer in time"
	}
	return err.Error()
}

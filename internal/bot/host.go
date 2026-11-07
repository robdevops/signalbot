package bot

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"time"

	"github.com/robdevops/signalbot/internal/chats"
	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/state"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// The ports. Features call them while they register, before Run.

func (b *Bot) OnMessage(fn func(context.Context, feature.Message) feature.Action) {
	b.messages = append(b.messages, fn)
}

func (b *Bot) OnTurn(fn func(context.Context, feature.Message) feature.Additions) {
	b.turns = append(b.turns, fn)
}

func (b *Bot) OnEvent(fn func(context.Context, feature.Event) string) {
	b.events = append(b.events, fn)
}

func (b *Bot) IntroLine(line string, dmOnly bool) {
	b.more = append(b.more, introLine{line, dmOnly})
}

// Tools keeps g with only the tools the model's API can run, and drops a
// group whose tools it can run none of. A group with no tools at all, only
// Names, is kept for the prompt's list of what the bot has.
func (b *Bot) Tools(g feature.ToolGroup) {
	if b.ai != nil && len(g.Tools) > 0 {
		if g.Tools = b.ai.Usable(g.Tools); len(g.Tools) == 0 {
			return
		}
	}
	b.groups = append(b.groups, g)
}

func (b *Bot) At(name string, t time.Time, fn func(context.Context, time.Time)) (func(), error) {
	return b.timers.At(name, t, fn)
}

// The services.

func (b *Bot) Env(key, def string) string { return config.Env(key, def) }

// storeName is what a saved state may be called: it names a file.
var storeName = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// Store opens a feature's saved state in the state directory, checked the
// first time one is opened, so a bot whose features save nothing needs
// none. Each name may be opened once, and budget is the core's.
func (b *Bot) Store(name string) (feature.Store, error) {
	if !storeName.MatchString(name) {
		return nil, fmt.Errorf("a saved state can't be called %q: use lowercase letters, digits, - and _, up to 32", txt.Clamp(name, 40))
	}
	if name == budgetName || slices.Contains(b.stores, name) {
		return nil, fmt.Errorf("a saved state called %q is already open", name)
	}
	if len(b.stores) == 0 {
		if err := state.Check(state.Dir(b.cfg.StateDir)); err != nil {
			return nil, err
		}
	}
	b.stores = append(b.stores, name)
	return state.Open(state.Dir(b.cfg.StateDir), name), nil
}

func (b *Bot) Info() feature.Info {
	info := feature.Info{Name: b.net.BotName(), Version: b.cfg.Version, Model: b.cfg.Model,
		Images: b.images.Load(), Audio: b.audio.Load(), Files: b.files.Load(), CallsTools: b.callsTools.Load(), Tools: b.toolNames(false), Intro: b.intro(false), IntroDM: b.intro(true)}
	if b.ai == nil {
		info.Model = ""
	}
	return info
}

func (b *Bot) Go(fn func(context.Context)) {
	b.running.Go(func() { fn(b.ctx) })
}

func (b *Bot) Media(ctx context.Context, msg feature.Message, i int) ([]byte, error) {
	return b.net.Media(ctx, msg, i)
}

func (b *Bot) Redact(text string) string { return b.net.Redact(text) }

func (b *Bot) Groups(ctx context.Context, user string) ([]feature.Group, error) {
	return b.net.Groups(ctx, user)
}

// Deliver acts on msg, in its chat's mailbox, as if its sender had just
// posted it.
func (b *Bot) Deliver(msg feature.Message) {
	if !b.closing.Load() {
		b.post(Received{Message: msg})
	}
}

// event runs the OnEvent hooks in its chat's slot, and sends the first
// non-empty reply, with @Name linked to the chat's known names and the
// event's people, so a welcome can mention a newcomer.
func (b *Bot) event(e feature.Event) {
	for _, fn := range b.events {
		reply := fn(b.ctx, e)
		if reply == "" {
			continue
		}
		known := b.chats.Names(e.Chat.Key())
		if known == nil {
			known = map[string]string{}
		}
		people := []feature.Person{e.By}
		if m := e.Members; m != nil {
			people = append(append(people, m.Joined...), m.Left...)
		}
		for _, p := range people {
			chats.Claim(known, p.Name, p.ID)
		}
		b.net.Send(e.Chat, reply, nil, known)
		return
	}
}

// delivered handles a delivered message in its chat's slot. One that
// mentions the bot, or that a hook acts on, is answered like any addressed
// message; anything else the bot posts as written, with @Name linked to
// the group's members.
func (b *Bot) delivered(msg feature.Message) {
	r, ok := b.net.Delivered(b.ctx, msg)
	if !ok {
		return
	}
	msg = r.Message
	key := msg.Chat.Key()
	if msg.Chat.GroupID != "" {
		b.chats.Learn(key, msg.Name, msg.Sender)
	}
	msg.Addressed = true
	if act := b.onMessage(b.ctx, msg); act != (feature.Action{}) || r.Mentioned {
		b.respond(msg, act, prompt(msg))
		return
	}
	known := map[string]string{}
	for _, m := range r.Members {
		chats.Claim(known, m.Name, m.ID)
	}
	b.net.Send(msg.Chat, msg.Text, nil, known)
	if b.ai != nil { // with no model, nothing would read it
		b.chats.Add(key, time.Now(), msg.Expires, chats.Entry{Role: "user", Content: cmp.Or(msg.Name, "someone") + ": " + msg.Text, Context: true})
	}
}

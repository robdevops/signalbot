package bot

import (
	"cmp"
	"context"
	"time"

	"github.com/robdevops/signalbot/internal/chats"
	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/feature"
)

// The ports. Features call them while they register, before Run.

func (b *Bot) OnMessage(fn func(context.Context, feature.Message) feature.Action) {
	b.messages = append(b.messages, fn)
}

func (b *Bot) OnTurn(fn func(context.Context, feature.Turn) feature.Additions) {
	b.turns = append(b.turns, fn)
}

func (b *Bot) OnReply(fn func(feature.Turn, *feature.Reply)) { b.replies = append(b.replies, fn) }

// Tools keeps g with only the tools the model's API can run, and drops a
// group whose tools it can run none of.
func (b *Bot) Tools(g feature.ToolGroup) {
	if b.ai != nil {
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

func (b *Bot) Info(feature.Chat) feature.Info {
	info := feature.Info{Name: b.net.BotName(), Version: b.cfg.Version, Model: b.cfg.Model,
		Images: b.images, CallsTools: b.callsTools, Tools: b.toolNames()}
	if b.ai == nil {
		info.Model = ""
	}
	return info
}

func (b *Bot) Go(fn func(context.Context)) {
	b.running.Add(1)
	go func() { defer b.running.Done(); fn(b.ctx) }()
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
		b.post(feature.Received{Message: msg, Delivered: true})
	}
}

// delivered handles a delivered message in its chat's slot. One that
// mentions the bot, or that a hook acts on, is answered like any addressed
// message; anything else the bot posts as written, with @Name linked to
// the group's members.
func (b *Bot) delivered(msg feature.Message) {
	known := map[string]string{}
	msg, mentioned, ok := b.net.Delivered(b.ctx, msg, func(name, id string) { chats.Claim(known, name, id) })
	if !ok {
		return
	}
	key := msg.Chat.Key()
	if msg.Chat.GroupID != "" {
		b.chats.Learn(key, msg.Name, msg.Sender)
	}
	msg.Addressed = true
	if act := b.onMessage(b.ctx, msg); act != (feature.Action{}) || mentioned {
		b.respond(msg, act, prompt(msg))
		return
	}
	b.net.Send(msg.Chat, msg.Text, nil, known)
	if b.ai != nil { // with no model, nothing would read it
		b.chats.Add(key, time.Now(), msg.Expires, chats.Entry{Role: "user", Content: cmp.Or(msg.Name, "someone") + ": " + msg.Text})
	}
}

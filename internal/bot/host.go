package bot

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"github.com/robdevops/signalbot/internal/chats"
	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/signal"
)

// The ports. Features call them while they register, before Run.

func (b *Bot) OnMessage(fn func(context.Context, feature.Message) feature.Action) {
	b.messages = append(b.messages, fn)
}

func (b *Bot) OnTurn(fn func(context.Context, feature.Turn) feature.Additions) {
	b.turns = append(b.turns, fn)
}

func (b *Bot) OnReply(fn func(feature.Turn, *feature.Reply)) { b.replies = append(b.replies, fn) }

func (b *Bot) Tools(g feature.ToolGroup) { b.groups = append(b.groups, g) }

func (b *Bot) At(name string, t time.Time, fn func(context.Context, time.Time)) (func(), error) {
	return b.timers.At(name, t, fn)
}

// The services.

func (b *Bot) Env(key, def string) string { return config.Env(key, def) }

func (b *Bot) Info(feature.Chat) feature.Info {
	return feature.Info{Name: b.sig.BotName(), Model: b.cfg.Model, Images: b.images, CallsTools: b.callsTools, Tools: b.toolNames()}
}

func (b *Bot) Media(ctx context.Context, msg feature.Message, i int) ([]byte, error) {
	if i < 0 || i >= len(msg.Media) {
		return nil, errors.New("no such media")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if m := msg.Media[i]; m.Pack != "" {
		return b.sig.Sticker(ctx, m.Pack, m.Sticker)
	}
	return b.sig.Attachment(ctx, msg.Chat, msg.Media[i].ID)
}

func (b *Bot) Groups(ctx context.Context, user string) ([]feature.Group, error) {
	gs, err := b.sig.Groups(ctx)
	var out []feature.Group
	for _, g := range gs {
		if g.Has(user) {
			out = append(out, g.Group)
		}
	}
	return out, err
}

// Deliver acts on msg, in its chat's mailbox, as if its sender had just
// posted it.
func (b *Bot) Deliver(msg feature.Message) {
	if !b.closing.Load() {
		b.post(msg.Chat.Key(), func() { b.delivered(msg) })
	}
}

// delivered handles a delivered message in its chat's slot. One that
// mentions the bot, or that a hook acts on, is answered like any addressed
// message; anything else the bot posts as written.
func (b *Bot) delivered(msg feature.Message) {
	msg.Name = cmp.Or(msg.Name, b.sig.Name(msg.Sender))
	key := msg.Chat.Key()
	known := map[string]string{} // the group's members by name, to link @Name
	if msg.Chat.GroupID != "" {
		ctx, cancel := context.WithTimeout(b.ctx, 10*time.Second)
		gs, err := b.sig.Groups(ctx)
		cancel()
		if err != nil {
			logx.Warnf("delivered message dropped: listGroups failed: %v", err)
			return
		}
		i := slices.IndexFunc(gs, func(g signal.Group) bool { return g.ID == msg.Chat.GroupID && g.Has(msg.Sender) })
		if i < 0 {
			logx.Infof("delivered message dropped: its sender is not in the group")
			return
		}
		for _, m := range gs[i].Members {
			chats.Claim(known, b.sig.Name(m.UUID, m.Number), m.UUID) // a member without an ACI is never linked
		}
		b.chats.Learn(key, msg.Name, msg.Sender)
	}
	text, mentioned := signal.StripAt(msg.Text, b.sig.BotName(), signal.DefaultName)
	msg.Text, msg.Addressed = text, true
	if act := b.onMessage(b.ctx, msg); act != (feature.Action{}) || mentioned {
		b.respond(msg, act)
		return
	}
	b.send(msg, msg.Text, nil, known)
	b.chats.Add(key, time.Now(), msg.Expires, chats.Entry{Role: "user", Content: cmp.Or(msg.Name, "someone") + ": " + msg.Text})
}

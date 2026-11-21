package bot

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/robdevops/signalbot/internal/chats"
	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/state"
	"github.com/robdevops/signalbot/internal/kit/txt"
	"github.com/robdevops/signalbot/internal/model"
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

func (b *Bot) Command(name string, fn func(context.Context, feature.Message, string) feature.Action) error {
	if err := b.commands.Take(name); err != nil {
		return err
	}
	b.OnMessage(feature.CommandHook(name, fn))
	return nil
}

// ListCommands adds a group with no tools, only its name, which is kept for
// the prompt's list of what the bot has and left out of {tools}.
func (b *Bot) ListCommands(label string, names ...string) {
	if len(names) > 0 {
		b.groups = append(b.groups, feature.ToolGroup{Names: []string{"**" + label + "**: /" + strings.Join(names, ", /")}})
	}
}

func (b *Bot) IntroLine(line string)   { b.introLines = append(b.introLines, introLine{line, false}) }
func (b *Bot) IntroLineDM(line string) { b.introLines = append(b.introLines, introLine{line, true}) }

// OfferTools keeps g with only the tools the model's API can run, and drops
// a group whose tools it can run none of.
func (b *Bot) OfferTools(g feature.ToolGroup) (kept bool) {
	if b.ai != nil && len(g.Tools) > 0 {
		if g.Tools = b.ai.Usable(g.Tools); len(g.Tools) == 0 {
			return false
		}
	}
	b.groups = append(b.groups, g)
	return true
}

func (b *Bot) At(name string, t time.Time, fn func(context.Context, time.Time)) (func(), error) {
	return b.timers.At(name, t, fn)
}

// The services.

func (b *Bot) Env(key, def string) string { return config.Env(key, def) }

// Store opens a feature's saved state in the state directory, checked the
// first time one is opened, so a bot whose features save nothing needs
// none. Each name may be opened once, and budget is the core's.
func (b *Bot) Store(name string) (feature.Store, error) {
	if err := state.CheckName(name); err != nil {
		return nil, err
	}
	if slices.Contains(b.stores, name) {
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
	info := feature.Info{Name: b.net.BotName(), Network: b.net.Name(), Version: b.cfg.Version, Model: b.cfg.Model,
		Images: b.images.Load(), Audio: b.audio.Load(), PDFs: b.files.Load(), CallsTools: b.callsTools.Load(), Tools: b.toolNames(false), Intro: b.intro(false), IntroDM: b.intro(true)}
	if b.ai == nil {
		info.Model = ""
	} else {
		info.Provider = b.ai.Provider()
	}
	return info
}

func (b *Bot) CallAPI(ctx context.Context, path string, body map[string]any) (json.RawMessage, error) {
	if b.ai == nil {
		return nil, errors.New("no model")
	}
	chat, charged := feature.ChargedChat(ctx)
	if model.Metered(ctx) || !charged { // a tool's cost counts toward its reply; with neither, Call refuses
		return b.ai.Call(ctx, path, body)
	}
	key := chat.Key()
	if b.budget != nil {
		if over, _ := b.budget.over(key); over {
			return nil, errOverBudget
		}
	}
	ctx, spent := model.Meter(ctx)
	data, err := b.ai.Call(ctx, path, body)
	if cost, ok := spent(); ok && b.budget != nil {
		b.budget.add(key, cost)
	}
	return data, err
}

// errOverBudget refuses a call charged to a chat that has spent its daily
// budget.
var errOverBudget = errors.New("the chat has reached its daily budget")

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
		b.post(Posted{Message: msg})
	}
}

// Post sends p from the bot, in its chat's mailbox. Its OnSent is called
// once, whatever happens to it.
func (b *Bot) Post(p feature.Post) {
	p.OnSent = once(p.OnSent)
	switch {
	case b.closing.Load(), p.Text == "" && len(p.Pictures) == 0:
		p.OnSent(nil, true)
	default:
		b.post(Outgoing{Post: p})
	}
}

// once is onSent made safe to call more than once, the calls after the
// first doing nothing; nil does nothing.
func once(onSent func(sent []int64, dropped bool)) func(sent []int64, dropped bool) {
	var o sync.Once
	return func(sent []int64, dropped bool) {
		o.Do(func() {
			if onSent != nil {
				onSent(sent, dropped)
			}
		})
	}
}

// say sends a post in its chat's slot, if the bot may still post there,
// with @Name linked to the chat's known names. With a label, history keeps
// it as the bot's own entry, "[Label] text", clamped, unless the send
// fails; without one, nothing.
func (b *Bot) say(p feature.Post) {
	defer p.OnSent(nil, true) // if it is never sent
	if !b.net.Reachable(b.ctx, p.Chat) {
		logx.Infof("dropped a post: its chat isn't reachable")
		return
	}
	key := p.Chat.Key()
	text, kept := captioned(p.Text, p.Pictures)
	if p.Label == "" || b.ai == nil { // with no model, nothing would read it
		sent := b.net.Send(p.Chat, text, p.Pictures, b.chats.Names(key), p.ReplyTo)
		p.OnSent(sent, len(sent) == 0)
		return
	}
	ref := b.pending()
	b.chats.Add(key, b.now(), 0, chats.Entry{Role: "assistant", Content: txt.Clamp("["+p.Label+"] "+kept, maxPostKept), Refs: []chats.Ref{ref}})
	sent := b.net.Send(p.Chat, text, p.Pictures, b.chats.Names(key), p.ReplyTo)
	if len(sent) == 0 {
		b.chats.Withdraw(key, ref.ID) // the chat never saw it, and it may be posted again
	} else {
		b.chats.Retag(key, ref.ID, botRefs(sent))
	}
	p.OnSent(sent, len(sent) == 0)
}

// maxPostKept caps what history keeps of a post.
const maxPostKept = 500

// React puts emoji on msg, in chat's mailbox at low priority, merged with
// the reactions already waiting there: a later one on the same message
// replaces the earlier, and past maxReactions the oldest is dropped.
func (b *Bot) React(chat feature.Chat, msg feature.Message, emoji string) {
	if b.closing.Load() || emoji == "" || msg.Timestamp == 0 { // a message the bot made can't be reacted to
		return
	}
	msg.Chat = chat
	add := Reacted{Chat: chat, Reactions: []Reaction{{Message: msg, Emoji: emoji}}}
	if err := b.chats.PostLow(chat.Key(), add, mergeReactions); err != nil {
		b.dropped(add, err)
	}
}

// mergeReactions adds m's reactions to those waiting: one on a message
// that already has one waiting replaces it, and past maxReactions the
// oldest is dropped.
func mergeReactions(waiting, m Received) Received {
	w := waiting.(Reacted)
	w.Reactions = slices.Clone(w.Reactions)
	for _, r := range m.(Reacted).Reactions {
		w.Reactions = slices.DeleteFunc(w.Reactions, func(x Reaction) bool {
			return x.Message.Sender == r.Message.Sender && x.Message.Timestamp == r.Message.Timestamp
		})
		if len(w.Reactions) >= maxReactions {
			logx.Warnf("dropped a reaction: %d already wait in its chat", maxReactions)
			w.Reactions = w.Reactions[1:]
		}
		w.Reactions = append(w.Reactions, r)
	}
	return w
}

// maxReactions caps the reactions waiting in one chat.
const maxReactions = 16

// react puts the waiting reactions on their messages in their chat's slot,
// skipping any message deleted or replaced by an edit since.
func (b *Bot) react(r Reacted) {
	for _, x := range r.Reactions {
		if !b.gone(x.Message) {
			b.net.React(r.Chat, x.Message, x.Emoji)
		}
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
		b.net.Send(e.Chat, reply, nil, known, 0)
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
	if act := b.hooked(msg); !act.IsZero() || r.Mentioned {
		b.respond(msg, act, prompt(msg), nil)
		return
	}
	known := map[string]string{}
	for _, m := range r.Members {
		chats.Claim(known, m.Name, m.ID)
	}
	p := b.pending()
	if b.ai != nil { // with no model, nothing would read it
		b.chats.Add(key, b.now(), msg.Expires, chats.Entry{Role: "user", Content: cmp.Or(msg.Name, "someone") + ": " + msg.Text, Context: true, Refs: []chats.Ref{p}})
	}
	b.sendKept(key, p, msg.Chat, msg.Text, nil, known)
}

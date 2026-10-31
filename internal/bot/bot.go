// Package bot is the core: it decides which messages to answer, asks the
// model and sends the reply, runs the features' hooks, and offers them the
// host's services.
package bot

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/robdevops/signalbot/internal/chats"
	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/openrouter"
	"github.com/robdevops/signalbot/internal/signal"
	"github.com/robdevops/signalbot/internal/timers"
)

// Bot is the core, and the Host features register on.
type Bot struct {
	cfg      *config.Config
	sig      *signal.Client
	ai       *openrouter.Client
	chats    *chats.Store
	timers   *timers.Timers
	slots    chan struct{} // one per message being handled
	ctx      context.Context
	stop     context.CancelFunc // stops the timers
	timerCtx context.Context
	closing  atomic.Bool // no new work starts

	images, callsTools bool // what the model can do, read when Run starts

	// The hooks and tool groups, in feature order. Features register them
	// before Run.
	messages []func(context.Context, feature.Message) feature.Action
	turns    []func(context.Context, feature.Turn) feature.Additions
	replies  []func(feature.Turn, *feature.Reply)
	groups   []feature.ToolGroup
}

var _ feature.Host = (*Bot)(nil)

// New makes the bot. Work in progress ends with ctx, which should outlive
// the shutdown signal so that Shutdown can let replies finish.
func New(ctx context.Context, cfg *config.Config) *Bot {
	b := &Bot{cfg: cfg, sig: signal.New(cfg.Socket), ai: openrouter.New(cfg), chats: chats.New(cfg.HistoryTurns),
		timers: timers.New(), slots: make(chan struct{}, cfg.MaxConcurrency), ctx: ctx}
	b.timerCtx, b.stop = context.WithCancel(ctx)
	return b
}

// Run reads what the model can do, then runs the timers and handles every
// message signal-cli receives, until the bot's context ends.
func (b *Bot) Run() {
	b.images, b.callsTools = b.ai.Capabilities(b.ctx)
	logx.Noticef("model %s, reasoning %s, images %v, tools %v", b.cfg.Model, b.cfg.Reasoning, b.images, b.callsTools)
	if !b.callsTools {
		logx.Warnf("model %s can't call tools, so tools are off", b.cfg.Model)
	}
	done := make(chan struct{})
	go func() { defer close(done); b.timers.Run(b.timerCtx) }()
	b.sig.Run(b.ctx, b.Handle)
	b.stop()
	<-done
}

// Shutdown stops the timers and new work, and waits up to d for the work in
// flight. Each piece of work holds a slot, so once Shutdown holds them all,
// none is left.
func (b *Bot) Shutdown(d time.Duration) {
	b.stop()
	b.closing.Store(true)
	timeout := time.After(d)
	for range cap(b.slots) {
		select {
		case b.slots <- struct{}{}:
		case <-timeout:
			logx.Warnf("shutdown timed out with replies still in flight")
			return
		}
	}
}

// Handle runs on the reader goroutine for every message received, so it
// never blocks or calls the network.
func (b *Bot) Handle(env *signal.Envelope) {
	d := env.DataMessage
	if d == nil || b.closing.Load() {
		return
	}
	if g := d.GroupInfo; g != nil && g.Type == "UPDATE" {
		b.chats.ForgetNames(feature.Chat{GroupID: g.GroupID}.Key()) // the signal client reloads the contacts
	}
	key := env.Chat().Key()
	if self := b.sig.ID(); self != "" {
		if b.route(env, self) {
			b.post(key, func() { b.received(env, self) })
		}
		return
	}
	// Until the bot's own ID is known, nothing can be judged: wait in a slot.
	// This job already holds the chat's worker, so an addressed message is
	// handled here, in order, rather than queued again.
	b.post(key, func() {
		ctx, cancel := context.WithTimeout(b.ctx, 10*time.Second)
		defer cancel()
		if self := b.sig.WaitID(ctx); self != "" && b.route(env, self) {
			b.received(env, self)
		}
	})
}

// route reports whether env is addressed, to be handled in its chat's slot.
// An unaddressed group message goes to the OnMessage hooks here. Neither is
// logged; a DM from someone not allowed is logged by ID only.
func (b *Bot) route(env *signal.Envelope, self string) bool {
	allowed := b.cfg.AllowedSenders
	switch {
	case signal.Addressed(env, self, allowed):
		return true
	case signal.FromSelf(env, self):
	case env.Chat().GroupID == "":
		if !signal.Allowed(env, allowed) {
			logx.Infof("%s ignored because not in SIGNAL_DM_USER_IDS", cmp.Or(env.Sender(), "unknown sender"))
		}
	case len(b.messages) > 0:
		msg := b.sig.Message(env, self, allowed)
		if a := b.onMessage(b.ctx, msg); a.Context != "" && !a.Ignore {
			b.chats.Add(msg.Chat.Key(), time.Now(), msg.Expires, chats.Entry{Role: "user", Content: a.Context})
		}
	}
	return false
}

// post queues job in the chat's mailbox, taking a slot for the chat's
// worker if it has none. Queued work is skipped once shutdown begins.
func (b *Bot) post(key string, job func()) {
	err := b.chats.Post(key, func() {
		if !b.closing.Load() {
			job()
		}
	}, func(worker func()) bool {
		select {
		case b.slots <- struct{}{}:
			go func() { defer func() { <-b.slots }(); worker() }()
			return true
		default:
			return false
		}
	})
	switch {
	case errors.Is(err, chats.ErrNoSlot):
		logx.Warnf("at capacity (%d), dropping message", b.cfg.MaxConcurrency)
	case err != nil:
		logx.Warnf("%v, dropping message", err)
	}
}

// received handles an addressed message in its chat's slot.
func (b *Bot) received(env *signal.Envelope, self string) {
	b.sig.LoadNames(b.ctx, env)
	msg := b.sig.Message(env, self, b.cfg.AllowedSenders)
	key := msg.Chat.Key()
	if msg.Chat.GroupID != "" {
		logx.Infof("received group chat")
	} else {
		logx.Infof("received dm")
	}
	for _, p := range b.sig.People(env, self) {
		b.chats.Learn(key, p.Name, p.ID)
	}
	id := msg.Sender + "\x00" + msg.Text
	for _, m := range msg.Media { // two different photos are not a repeat
		id += fmt.Sprintf("\x00%s:%s:%d", m.ID, m.Pack, m.Sticker)
	}
	if b.chats.Repeat(key, id) {
		logx.Debugf("ignored duplicate from %s", cmp.Or(msg.Name, msg.Sender))
		return
	}
	for i := range msg.Media {
		if m := &msg.Media[i]; m.Pack != "" {
			ctx, cancel := context.WithTimeout(b.ctx, 10*time.Second)
			m.Label = b.sig.StickerLabel(ctx, m.Pack, m.Sticker)
			cancel()
		}
	}
	b.respond(msg, b.onMessage(b.ctx, msg))
}

// onMessage runs the OnMessage hooks in order. The first Action that is
// non-zero, counting only what applies to the message, wins.
func (b *Bot) onMessage(ctx context.Context, msg feature.Message) feature.Action {
	for _, fn := range b.messages {
		if a := fn(ctx, msg).For(msg); a != (feature.Action{}) {
			return a
		}
	}
	return feature.Action{}
}

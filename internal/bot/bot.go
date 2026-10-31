// Package bot is the core: it decides which messages to answer, asks the
// model and sends the reply, runs the features' hooks, and offers them the
// host's services.
package bot

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
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
	waitMu   sync.Mutex
	waiting  map[string][]*signal.Envelope // addressed messages queued per chat, answered together
	ctx      context.Context
	stop     context.CancelFunc // stops the timers
	timerCtx context.Context
	closing  atomic.Bool    // no new work starts
	running  sync.WaitGroup // what features run with Go

	images, callsTools bool // what the model can do, read by New

	// The hooks and tool groups, in feature order. Features register them
	// before Run.
	messages []func(context.Context, feature.Message) feature.Action
	turns    []func(context.Context, feature.Turn) feature.Additions
	replies  []func(feature.Turn, *feature.Reply)
	groups   []feature.ToolGroup
}

var _ feature.Host = (*Bot)(nil)

// New makes the bot and reads what the model can do, so features know it
// when they register. Work in progress ends with ctx, which should outlive
// the shutdown signal so that Shutdown can let replies finish.
func New(ctx context.Context, cfg *config.Config) *Bot {
	b := &Bot{cfg: cfg, sig: signal.New(cfg.Socket), ai: openrouter.New(cfg), chats: chats.New(cfg.HistoryTurns),
		timers: timers.New(), slots: make(chan struct{}, cfg.MaxConcurrency), ctx: ctx}
	b.timerCtx, b.stop = context.WithCancel(ctx)
	b.images, b.callsTools = b.ai.Capabilities(ctx)
	logx.Noticef("model %s, reasoning %s, images %v, tools %v", cfg.Model, cfg.Reasoning, b.images, b.callsTools)
	if !b.callsTools {
		logx.Warnf("model %s can't call tools, so tools are off", cfg.Model)
	}
	return b
}

// Run handles every message signal-cli receives, and runs the timers once
// it is first connected, until the bot's context ends, then waits for what
// features run with Go. A timer due at startup so waits for signal-cli
// however long it takes to come up, rather than delivering into nothing.
func (b *Bot) Run() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if b.sig.WaitID(b.timerCtx) != "" {
			b.timers.Run(b.timerCtx)
		}
	}()
	b.sig.Run(b.ctx, b.Handle)
	b.stop()
	<-done
	b.running.Wait()
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

// Idle reports whether no work holds a slot and the contact list isn't
// loading. It is for the scenario tests in cmd/signalbot.
func (b *Bot) Idle() bool { return len(b.slots) == 0 && !b.sig.Loading() }

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
			b.queue(key, env, self)
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
func (b *Bot) post(key string, job func()) error {
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
	return err
}

// queue adds an addressed message to its chat's waiting list. The first
// message waiting posts the one job that answers the whole list, so a
// message that arrives while the bot is answering an earlier one is
// answered with the others that arrived meanwhile: "actually" and "forget
// it" get one reply that sees both. At most chats.MaxQueue wait.
func (b *Bot) queue(key string, env *signal.Envelope, self string) {
	b.waitMu.Lock() // held across post, so a failed post never races the job
	defer b.waitMu.Unlock()
	w := b.waiting[key]
	if len(w) >= chats.MaxQueue {
		logx.Warnf("%v, dropping message", chats.ErrQueueFull)
		return
	}
	if b.waiting == nil {
		b.waiting = map[string][]*signal.Envelope{}
	}
	b.waiting[key] = append(w, env)
	if len(w) == 0 && b.post(key, func() { b.answerWaiting(key, self) }) != nil {
		delete(b.waiting, key)
	}
}

// answerWaiting answers every message waiting in a chat, in order: one that
// a hook acts on (a command, an answer, an ignore) on its own, and runs of
// the rest together, as one turn, each run within one message's picture
// limits. A panic loses only the message or turn that caused it, and
// nothing new starts once shutdown begins.
func (b *Bot) answerWaiting(key, self string) {
	b.waitMu.Lock()
	envs := b.waiting[key]
	delete(b.waiting, key)
	b.waitMu.Unlock()
	var batch []feature.Message
	flush := func() {
		run := batch
		batch = nil
		if len(run) > 0 && !b.closing.Load() {
			safely(func() { b.respond(merge(run), feature.Action{}) })
		}
	}
	for _, env := range envs {
		if b.closing.Load() {
			return
		}
		safely(func() {
			msg, ok := b.load(env, self)
			if !ok {
				return
			}
			if act := b.onMessage(b.ctx, msg); act != (feature.Action{}) {
				flush()
				if !b.closing.Load() {
					b.respond(msg, act)
				}
				return
			}
			if !fits(batch, msg) {
				flush()
			}
			batch = append(batch, msg)
		})
	}
	flush()
}

// safely runs fn, logging a panic without its value rather than letting it
// end the rest of the work.
func safely(fn func()) {
	defer logx.Recover("handling a message")
	fn()
}

// pictures counts a message's images and stickers.
func pictures(m feature.Message) (images, stickers int) {
	for _, md := range m.Media {
		if md.Pack != "" {
			stickers++
		} else {
			images++
		}
	}
	return images, stickers
}

// fits reports whether msg can join batch and the merged message still
// carry what one message may: at most feature.MaxPictures images and one
// sticker.
func fits(batch []feature.Message, msg feature.Message) bool {
	images, stickers := pictures(msg)
	for _, x := range batch {
		i, s := pictures(x)
		images, stickers = images+i, stickers+s
	}
	return images <= feature.MaxPictures && stickers <= 1
}

// merge makes messages that arrived together one message: each as the model
// would see it (in a group with its sender's name and any quote), a line
// each, with their images and then their sticker. A message with neither
// text nor pictures adds nothing, as it would have nothing to answer on its
// own. One message is left as it is.
func merge(msgs []feature.Message) feature.Message {
	if len(msgs) == 1 {
		return msgs[0]
	}
	m := msgs[len(msgs)-1]
	var lines []string
	var images, stickers []feature.Media
	for _, x := range msgs {
		if x.Text == "" && len(x.Media) == 0 {
			continue
		}
		if l := prompt(x); l != "" {
			lines = append(lines, l)
		}
		for _, md := range x.Media {
			if md.Pack != "" {
				stickers = append(stickers, md)
			} else {
				images = append(images, md)
			}
		}
		if x.Expires > 0 && (m.Expires == 0 || x.Expires < m.Expires) {
			m.Expires = x.Expires
		}
	}
	m.Text, m.Quote, m.Media = strings.Join(lines, "\n"), nil, append(images, stickers...)
	if m.Chat.GroupID != "" {
		m.Name = "" // each line already names its sender
	}
	return m
}

// received handles an addressed message in its chat's slot.
func (b *Bot) received(env *signal.Envelope, self string) {
	if msg, ok := b.load(env, self); ok {
		b.respond(msg, b.onMessage(b.ctx, msg))
	}
}

// load turns an addressed message into what features see, in its chat's
// slot: names loaded, sticker labels filled in. It logs that it arrived,
// and reports false for a repeat.
func (b *Bot) load(env *signal.Envelope, self string) (feature.Message, bool) {
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
	// Neither a renamed sender nor two different photos are a repeat.
	id := msg.Sender + "\x00" + msg.Name + "\x00" + msg.Text
	for _, m := range msg.Media {
		id += fmt.Sprintf("\x00%s:%s:%d", m.ID, m.Pack, m.Sticker)
	}
	if b.chats.Repeat(key, id) {
		logx.Debugf("ignored duplicate from %s", cmp.Or(msg.Name, msg.Sender))
		return msg, false
	}
	for i := range msg.Media {
		if m := &msg.Media[i]; m.Pack != "" {
			ctx, cancel := context.WithTimeout(b.ctx, 10*time.Second)
			m.Label = b.sig.StickerLabel(ctx, m.Pack, m.Sticker)
			cancel()
		}
	}
	return msg, true
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

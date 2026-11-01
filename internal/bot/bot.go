// Package bot is the core: it decides which messages to answer, asks the
// model and sends the reply, runs the features' hooks, and offers them the
// host's services.
package bot

import (
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
	"github.com/robdevops/signalbot/internal/model"
	"github.com/robdevops/signalbot/internal/timers"
)

// Network is the chat network the bot answers on: Signal, or the bot's
// tests' fake. Media, Groups and Redact are the host's services of those
// names.
type Network interface {
	// Run calls handle on one goroutine for each message received, until
	// ctx ends. handle must not block.
	Run(ctx context.Context, handle func(feature.Received))
	// WaitID returns the bot's own ID once it is connected, or "" if ctx
	// ends first.
	WaitID(ctx context.Context) string
	// Send posts text to chat, linking each @Name in known (lowercased name
	// to ID), with the attachments (data: URIs). Failures are logged.
	Send(chat feature.Chat, text string, attachments []string, known map[string]string)
	// Typing shows the bot typing in chat until stop is called.
	Typing(chat feature.Chat) (stop func())
	// Delivered readies a message a feature delivers, as if its sender had
	// just posted it, passing claim the name and ID of each person in a
	// group's chat to link @Name to. It returns msg without the bot's
	// @name, and whether that was there; false, logged, if msg is dropped.
	Delivered(ctx context.Context, msg feature.Message, claim func(name, id string)) (out feature.Message, mentioned, ok bool)
	// BotName is the bot's own name. It never blocks.
	BotName() string
	Media(ctx context.Context, msg feature.Message, i int) ([]byte, error)
	Groups(ctx context.Context, user string) ([]feature.Group, error)
	Redact(text string) string
}

// Bot is the core, and the Host features register on.
type Bot struct {
	cfg      *config.Config
	net      Network
	ai       *model.Client // nil with no model
	chats    *chats.Store
	timers   *timers.Timers
	slots    chan struct{} // one per message being handled
	waitMu   sync.Mutex
	waiting  map[string][]feature.Received // addressed messages queued per chat, answered together
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

// New makes the bot on net and reads what the model can do, so features
// know it when they register. With no model, only what a hook answers is
// answered. Work in progress ends with ctx, which should outlive the
// shutdown signal so that Shutdown can let replies finish.
func New(ctx context.Context, cfg *config.Config, net Network) *Bot {
	b := &Bot{cfg: cfg, net: net, chats: chats.New(cfg.HistoryTurns),
		timers: timers.New(), slots: make(chan struct{}, cfg.MaxConcurrency), ctx: ctx}
	b.timerCtx, b.stop = context.WithCancel(ctx)
	if cfg.NoModel {
		logx.Noticef("no model: only commands are answered")
		return b
	}
	b.ai = model.New(cfg)
	b.images, b.callsTools = b.ai.Capabilities(ctx)
	logx.Noticef("model %s, reasoning %s, images %v, tools %v", cfg.Model, cfg.Reasoning, b.images, b.callsTools)
	if !b.callsTools {
		logx.Warnf("model %s can't call tools, so tools are off", cfg.Model)
	}
	return b
}

// Run handles every message the network receives, and runs the timers once
// it is first connected, until the bot's context ends, then waits for what
// features run with Go. A timer due at startup so waits for the network
// however long it takes to come up, rather than delivering into nothing.
func (b *Bot) Run() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		if b.net.WaitID(b.timerCtx) != "" {
			b.timers.Run(b.timerCtx)
		}
	}()
	b.net.Run(b.ctx, b.handle)
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

// Idle reports whether no work holds a slot. It is for the scenario tests
// at the module root.
func (b *Bot) Idle() bool { return len(b.slots) == 0 }

// handle runs on the network's reader goroutine for every message received,
// so it never blocks or calls the network. An unaddressed group message
// goes to the OnMessage hooks here, and is never logged; an addressed one
// is handled in its chat's slot.
func (b *Bot) handle(r feature.Received) {
	if b.closing.Load() {
		return
	}
	key := r.Chat.Key()
	switch {
	case r.GroupChange:
		b.chats.ForgetNames(key)
	case r.Wait:
		// Until the bot's own ID is known, nothing can be judged: wait in a
		// slot. This job already holds the chat's worker, so an addressed
		// message is handled here, in order, rather than queued again.
		b.post(key, func() {
			switch msg, ok := b.load(r); {
			case ok && msg.Addressed:
				b.respond(msg, b.onMessage(b.ctx, msg), prompt(msg))
			case ok:
				b.keep(msg)
			}
		})
	case r.Addressed:
		b.queue(key, r)
	case len(b.messages) > 0:
		b.keep(r.Message)
	}
}

// keep runs the OnMessage hooks for an unaddressed group message, and keeps
// the Context one returns in history.
func (b *Bot) keep(msg feature.Message) {
	if a := b.onMessage(b.ctx, msg); a.Context != "" && !a.Ignore && b.ai != nil { // with no model, nothing would read it
		b.chats.Add(msg.Chat.Key(), time.Now(), msg.Expires, chats.Entry{Role: "user", Content: a.Context})
	}
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
func (b *Bot) queue(key string, r feature.Received) {
	b.waitMu.Lock() // held across post, so a failed post never races the job
	defer b.waitMu.Unlock()
	w := b.waiting[key]
	if len(w) >= chats.MaxQueue {
		logx.Warnf("%v, dropping message", chats.ErrQueueFull)
		return
	}
	if b.waiting == nil {
		b.waiting = map[string][]feature.Received{}
	}
	b.waiting[key] = append(w, r)
	if len(w) == 0 && b.post(key, func() { b.answerWaiting(key) }) != nil {
		delete(b.waiting, key)
	}
}

// answerWaiting answers every message waiting in a chat, in order: one that
// a hook acts on (a command, an answer, an ignore) on its own, and runs of
// the rest together, as one turn, each run within one message's picture
// limits. A panic loses only the message or turn that caused it, and
// nothing new starts once shutdown begins.
func (b *Bot) answerWaiting(key string) {
	b.waitMu.Lock()
	rs := b.waiting[key]
	delete(b.waiting, key)
	b.waitMu.Unlock()
	var batch []feature.Message
	flush := func() {
		run := batch
		batch = nil
		if len(run) > 0 && !b.closing.Load() {
			safely(func() { m, text := merge(run); b.respond(m, feature.Action{}, text) })
		}
	}
	for _, r := range rs {
		if b.closing.Load() {
			return
		}
		safely(func() {
			msg, ok := b.load(r)
			if !ok || !msg.Addressed {
				return
			}
			if act := b.onMessage(b.ctx, msg); act != (feature.Action{}) {
				flush()
				if !b.closing.Load() {
					b.respond(msg, act, prompt(msg))
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

// merge makes messages that arrived together one message, with their
// images and then their sticker, and returns it with the text the model
// sees: each message as it would see it (in a group with its sender's name
// and any quote), a line each. The message's own Text is only what was
// typed, a line each, so tool keywords never match a name or a quote. A
// message with neither text nor pictures adds nothing, as it would have
// nothing to answer on its own. One message is left as it is.
func merge(msgs []feature.Message) (feature.Message, string) {
	if len(msgs) == 1 {
		return msgs[0], prompt(msgs[0])
	}
	m := msgs[len(msgs)-1]
	var lines, typed []string
	var images, stickers []feature.Media
	for _, x := range msgs {
		if x.Text == "" && len(x.Media) == 0 {
			continue
		}
		if l := prompt(x); l != "" {
			lines = append(lines, l)
		}
		if x.Text != "" {
			typed = append(typed, x.Text)
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
	m.Text, m.Quote, m.Media = strings.Join(typed, "\n"), nil, append(images, stickers...)
	if m.Chat.GroupID != "" {
		m.Name = "" // each line already names its sender
	}
	return m, strings.Join(lines, "\n")
}

// load completes a message in its chat's slot. An addressed one is logged
// as it arrives, and reported false if it is a repeat.
func (b *Bot) load(r feature.Received) (feature.Message, bool) {
	key := r.Chat.Key()
	msg, ok := r.Load(b.ctx, func(name, id string) { b.chats.Learn(key, name, id) })
	if !ok || !msg.Addressed {
		return msg, ok // only one that arrived before the bot's ID was known can be unaddressed
	}
	if msg.Chat.GroupID != "" {
		logx.Infof("received group chat")
	} else {
		logx.Infof("received dm")
	}
	// Neither a renamed sender nor two different photos are a repeat.
	id := msg.Sender + "\x00" + msg.Name + "\x00" + msg.Text
	for _, m := range msg.Media {
		id += fmt.Sprintf("\x00%s:%s:%d", m.ID, m.Pack, m.Sticker)
	}
	if b.chats.Repeat(key, id) {
		logx.Debugf("ignored duplicate")
		return msg, false
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

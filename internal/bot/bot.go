// Package bot is the core: it decides which messages to answer, asks the
// model and sends the reply, runs the features' hooks, and offers them the
// host's services.
package bot

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

// Network is the chat network the bot answers on, or the bot's tests'
// fake. Media, Groups and Redact are the host's services of those
// names.
type Network interface {
	// Run calls handle for each message received until ctx ends. handle
	// must not block.
	Run(ctx context.Context, handle func(Received))
	// Watch says whether any feature has an event hook, before Run, so the
	// network can skip work for events nothing would read.
	Watch(events bool)
	WaitID(ctx context.Context) string
	Send(chat feature.Chat, text string, pics []feature.Picture, known map[string]string)
	// React puts emoji on msg in chat, as a reaction from the bot.
	React(chat feature.Chat, msg feature.Message, emoji string)
	Typing(chat feature.Chat) (stop func())
	// Delivered readies a posted message in its slot. ok is false, logged,
	// if msg is dropped.
	Delivered(ctx context.Context, msg feature.Message) (r Ready, ok bool)
	// BotName never blocks.
	BotName() string
	// Prompt is the network's own line in the system prompt, such as its
	// message limit.
	Prompt() string
	Media(ctx context.Context, msg feature.Message, i int) ([]byte, error)
	Groups(ctx context.Context, user string) ([]feature.Group, error)
	Redact(text string) string
}

// Member is a group member: a display name and the network's ID for them,
// "" if they can never be linked.
type Member struct{ Name, ID string }

// Bot is the core, and the Host features register on.
type Bot struct {
	cfg        *config.Config
	net        Network
	ai         *model.Client // nil with no model
	chats      *chats.Store[Received]
	timers     *timers.Timers
	slots      chan struct{} // one per chat being worked on
	ctx        context.Context
	stopTimers context.CancelFunc // stops the timers
	timerCtx   context.Context
	closing    atomic.Bool    // no new work starts
	running    sync.WaitGroup // the timers, and what features run with Go

	// What the model can do, read by New. Each turns off for the rest of
	// the run when the API rejects a request carrying it; audio goes with
	// files.
	images, audio, files, callsTools atomic.Bool
	budget                           *budget // nil with no cap

	// The hooks and tool groups, in feature order. Features register them
	// before Run.
	messages   []func(context.Context, feature.Message) feature.Action
	turns      []func(context.Context, feature.Message) feature.Additions
	groups     []feature.ToolGroup
	commands   feature.Commands // the command names taken
	introLines []introLine      // features' lines for the introduction
	stores     []string         // the names of the saved states features opened
	events     []func(context.Context, feature.Event) string
	now        func() time.Time // the clock history and the budget go by
}

// New makes the bot on net and reads what the model can do, so features
// know it when they register. With no model, only what a hook answers is
// answered. Work in progress ends with ctx, which should outlive the
// shutdown signal so that Shutdown can let replies finish.
func New(ctx context.Context, cfg *config.Config, net Network) *Bot {
	b := &Bot{cfg: cfg, net: net, timers: timers.New(), slots: make(chan struct{}, cfg.MaxConcurrency), ctx: ctx, now: time.Now}
	b.chats = chats.New(cfg.HistoryMax, b.work, b.spawn)
	b.timerCtx, b.stopTimers = context.WithCancel(ctx)
	if cfg.NoModel {
		logx.Warnf("no model: set MODEL_API_KEY (or MODEL_API_URL for a provider that needs no key); until then only commands are answered")
		return b
	}
	b.ai = model.New(cfg)
	caps := b.ai.Capabilities(ctx)
	b.images.Store(caps.Images)
	b.audio.Store(caps.Audio)
	b.files.Store(caps.PDFs)
	b.callsTools.Store(caps.CallsTools)
	logx.Noticef("model %s, reasoning %s, images %v, audio %v, PDFs %v, tools %v", cfg.Model, cfg.Reasoning, caps.Images, caps.Audio, caps.PDFs, caps.CallsTools)
	if !caps.CallsTools {
		logx.Warnf("model %s can't call tools, so tools are off", cfg.Model)
	}
	if cfg.Test != nil && cfg.Test.Now != nil {
		b.now = cfg.Test.Now
		b.chats.SetClock(b.now)
	}
	switch {
	case cfg.DailyBudget <= 0:
	case !b.ai.ReportsCost() && cfg.DailyBudgetSet:
		logx.Warnf("DAILY_BUDGET=%g is ignored: only OpenRouter reports what a reply costs, so spending is not capped", cfg.DailyBudget)
	case !b.ai.ReportsCost():
		logx.Noticef("DAILY_BUDGET isn't applied: only OpenRouter reports what a reply costs")
	default:
		b.budget = newBudget(cfg.StateDir, cfg.DailyBudget, b.now)
		logx.Noticef("daily budget $%.2f per chat", cfg.DailyBudget)
	}
	return b
}

// Run handles every message the network receives, and runs the timers once
// it is first connected, until the bot's context ends, then waits for what
// features run with Go. A timer due at startup so waits for the network
// however long it takes to come up, rather than delivering into nothing.
func (b *Bot) Run() {
	b.running.Go(func() {
		if b.net.WaitID(b.timerCtx) != "" {
			b.timers.Run(b.timerCtx)
		}
	})
	b.net.Watch(len(b.events) > 0)
	b.net.Run(b.ctx, b.handle)
	b.stopTimers()
	b.running.Wait()
}

// Shutdown stops the timers and new work, and waits up to d for the work in
// flight. Each piece of work holds a slot, so once Shutdown holds them all,
// none is left.
func (b *Bot) Shutdown(d time.Duration) {
	b.stopTimers()
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

// handle runs on the network's reader goroutine for everything received (a
// member change comes from another of its goroutines), so it never blocks
// or calls the network. An Incoming message and, with a hook for them, a
// Happened event go to their chat's mailbox; Chatter goes to the OnMessage
// hooks here, and is never logged. (A Posted message comes from Deliver,
// never from the network.)
func (b *Bot) handle(r Received) {
	if b.closing.Load() {
		return
	}
	switch r := r.(type) {
	case GroupChanged:
		b.chats.ForgetNames(r.Chat.Key())
	case Chatter:
		b.overhear(r.Message)
	case Happened:
		if len(b.events) > 0 { // with no hook, nothing would read it
			b.post(r)
		}
	case Incoming:
		b.post(r)
	}
}

// overhear runs the OnMessage hooks for an unaddressed group message, and keeps
// the Context one returns in history.
func (b *Bot) overhear(msg feature.Message) {
	if a := b.onMessage(b.ctx, msg); a.Context != "" && b.ai != nil { // with no model, nothing would read it
		b.chats.Add(msg.Chat.Key(), b.now(), msg.Expires, chats.Entry{Role: "user", Content: a.Context, Context: true})
	}
}

// spawn starts a chat's worker on a free slot, or returns false with none.
func (b *Bot) spawn(worker func()) bool {
	select {
	case b.slots <- struct{}{}:
		end := b.cfg.Test.Begin()
		go func() { defer func() { end(); <-b.slots }(); worker() }()
		return true
	default:
		return false
	}
}

// post adds r to its chat's mailbox.
func (b *Bot) post(r Received) {
	switch err := b.chats.Post(r.chat().Key(), r); {
	case errors.Is(err, chats.ErrNoSlot):
		logx.Warnf("dropped a message: all %d worker slots are busy (raise MAX_CONCURRENCY)", b.cfg.MaxConcurrency)
	case err != nil:
		logx.Warnf("dropped a message: %v", err)
	}
}

// work handles what waited in a chat's mailbox, in order. Addressed
// messages are answered together, as one turn, so "actually" and "forget
// it" get one reply that sees both; each run stays within one message's
// picture limits. On its own goes a message a hook acts on (a command or
// an answer), a delivered one, and one that waited for the bot's
// ID and was not addressed after all. A panic loses only the message or
// turn that caused it, and nothing new starts once shutdown begins.
func (b *Bot) work(rs []Received) {
	var batch []feature.Message
	flush := func() {
		run := batch
		batch = nil
		if len(run) > 0 && !b.closing.Load() {
			safely(func() { m, text := merge(run); b.respond(m, feature.Action{}, text) })
		}
	}
	alone := func(fn func()) {
		flush()
		if !b.closing.Load() {
			fn()
		}
	}
	for _, r := range rs {
		if b.closing.Load() {
			return
		}
		safely(func() {
			var p Incoming
			switch r := r.(type) {
			case Happened:
				alone(func() { b.event(r.Event) })
				return
			case Posted:
				alone(func() { b.delivered(r.Message) })
				return
			case Told:
				alone(func() { b.told(r) })
				return
			case Incoming:
				p = r
			default:
				return
			}
			msg, ok := b.load(p)
			switch {
			case !ok:
			case !msg.Addressed:
				alone(func() { b.overhear(msg) })
			default:
				if act := b.onMessage(b.ctx, msg); act != (feature.Action{}) {
					alone(func() { b.respond(msg, act, prompt(msg)) })
					return
				}
				if !fits(batch, msg) {
					flush()
				}
				batch = append(batch, msg)
			}
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

// pictures counts a message's images, other files and stickers.
func pictures(m feature.Message) (images, files, stickers int) {
	for _, md := range m.Media {
		switch {
		case md.Sticker:
			stickers++
		case md.File:
			files++
		default:
			images++
		}
	}
	return images, files, stickers
}

// fits reports whether msg can join batch and the merged message still
// carry what one message may: at most feature.MaxPictures images,
// feature.MaxFiles files and one sticker.
func fits(batch []feature.Message, msg feature.Message) bool {
	images, files, stickers := pictures(msg)
	for _, x := range batch {
		i, f, s := pictures(x)
		images, files, stickers = images+i, files+f, stickers+s
	}
	return images <= feature.MaxPictures && files <= feature.MaxFiles && stickers <= 1
}

// merge makes messages that arrived together one message, with their
// images, then their files, then their sticker, and returns it with the text the model
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
	var images, files, stickers []feature.Media
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
			switch {
			case md.Sticker:
				stickers = append(stickers, md)
			case md.File:
				files = append(files, md)
			default:
				images = append(images, md)
			}
		}
		if x.Expires > 0 && (m.Expires == 0 || x.Expires < m.Expires) {
			m.Expires = x.Expires
		}
	}
	m.Text, m.Quote, m.Media = strings.Join(typed, "\n"), nil, slices.Concat(images, files, stickers)
	if m.Chat.GroupID != "" {
		m.Name = "" // each line already names its sender
	}
	return m, strings.Join(lines, "\n")
}

// load completes a message in its chat's slot. An addressed one is logged
// as it arrives, and reported false if it is a repeat.
func (b *Bot) load(r Incoming) (feature.Message, bool) {
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
		id += fmt.Sprintf("\x00%s:%t", m.ID, m.Sticker)
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

// introLine is a feature's line for the introduction, perhaps only in a DM.
type introLine struct {
	text   string
	dmOnly bool
}

// Package bot is the core: it decides which messages to answer, asks the
// model and sends the reply, runs the features' hooks, and offers them the
// host's services.
package bot

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
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
	// Watch says, before Run, what the network should look out for: whether
	// any feature has an event hook, so it can skip work for events nothing
	// would read, and the commands features registered, so a group message
	// that starts with one counts as addressed to the bot.
	Watch(w Watch)
	WaitID(ctx context.Context) string
	// Send posts a reply, split as the network needs, and returns the
	// timestamps of the messages sent, which name them for a later delete;
	// a send that fails returns no timestamps. If replyTo isn't 0, the
	// first message sent replies to the bot's own message in chat that an
	// earlier Send returned it for; one the network can't find is sent
	// without it. audio, if not nil, is one MP3 clip sent with the text,
	// as audio to play, with no title of its own. mk is shown where the
	// network can (Caps), and left off where it can't.
	Send(chat feature.Chat, text string, pics []feature.Picture, known map[string]string, replyTo int64, audio []byte, mk feature.Markup) (sent []int64)
	// Edit replaces the bot's own message in chat that Send returned sent
	// for with text, pics and mk's buttons, and returns the timestamps that
	// now stand for it: sent, or a new message's where the network can't
	// edit it in place; none if it failed, which is logged.
	Edit(chat feature.Chat, sent int64, text string, pics []feature.Picture, known map[string]string, mk feature.Markup) []int64
	// Caps is what the network can show and do beyond plain messages; it
	// never blocks.
	Caps() feature.Caps
	// React puts emoji on msg in chat, as a reaction from the bot.
	React(chat feature.Chat, msg feature.Message, emoji string)
	Typing(chat feature.Chat) (stop func())
	// Delivered readies a posted message in its slot. ok is false, logged,
	// if msg is dropped.
	Delivered(ctx context.Context, msg feature.Message) (r Ready, ok bool)
	// Reachable reports, in the chat's slot, whether the bot may still post
	// in chat on its own: a group it is still in and answers in, or the DM
	// of a user still allowed to DM it.
	Reachable(ctx context.Context, chat feature.Chat) bool
	// Name is the network's own short lowercase name, such as the one its
	// package is named for; it never blocks.
	Name() string
	// BotName never blocks.
	BotName() string
	// Prompt is the network's own line in the system prompt, such as its
	// message limit.
	Prompt() string
	Media(ctx context.Context, msg feature.Message, i int) ([]byte, error)
	Groups(ctx context.Context, user string) ([]feature.Group, error)
	Redact(text string) string
}

// Watch is what the network should look out for, set once before Run.
type Watch struct {
	Events   bool      // a feature has an OnEvent hook
	Commands []Command // the commands features registered
}

// Command is a command a feature registered: its name, lowercase, without
// "/", and what it is about, for a network's command menu (Telegram's "/"
// menu): the label of its ListCommands group, or "" for none.
type Command struct{ Name, About string }

// Member is a group member: a display name and the network's ID for them,
// "" if they can never be linked.
type Member struct{ Name, ID string }

// Bot is the core, and the Host features register on.
type Bot struct {
	sends      atomic.Int64 // pending refs made, each its own
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
	commands   feature.Commands  // the command names taken
	about      map[string]string // a command's ListCommands label, by name
	introLines []introLine       // features' lines for the introduction
	stores     []string          // the names of the saved states features opened
	events     []func(context.Context, feature.Event) string
	presses    []func(context.Context, feature.Press) (string, bool)
	now        func() time.Time // the clock history and the budget go by
	keys       *keyboards       // each DM's reply keyboard
}

// New makes the bot on net and reads what the model can do, so features
// know it when they register. With no model, only what a hook answers is
// answered. Work in progress ends with ctx, which should outlive the
// shutdown signal so that Shutdown can let replies finish.
func New(ctx context.Context, cfg *config.Config, net Network) *Bot {
	b := &Bot{cfg: cfg, net: net, timers: timers.New(), slots: make(chan struct{}, cfg.MaxConcurrency), ctx: ctx, now: time.Now, keys: newKeyboards(cfg.StateDir)}
	b.chats = chats.New(cfg.HistoryMax, b.work, b.spawn)
	b.chats.SetEvicted(func(r Received) { b.dropped(r, errMadeRoom) })
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
	var cmds []Command
	for _, n := range b.commands.Names() {
		cmds = append(cmds, Command{Name: n, About: b.about[n]})
	}
	b.net.Watch(Watch{Events: len(b.events) > 0, Commands: cmds})
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
// Happened event and a Pressed button go to their chat's mailbox (a press
// with no hook is answered at once); Chatter goes to the OnMessage
// hooks here, and is never logged. (Posted, Outgoing and Reacted come from
// Deliver, Post and React, never from the network.)
func (b *Bot) handle(r Received) {
	if b.closing.Load() {
		return
	}
	switch r := r.(type) {
	case GroupChanged:
		b.chats.ForgetNames(r.Chat.Key())
	case Deleted:
		sender := r.Sender
		if r.Bot {
			sender = botSender
		}
		b.chats.Forget(r.Chat.Key(), ref(sender, r.Sent), r.At)
	case Chatter:
		b.overhear(r.Message)
	case Happened:
		if len(b.events) > 0 { // with no hook, nothing would read it
			b.post(r)
		}
	case Pressed:
		r.Answer = onceAnswer(r.Answer)
		if len(b.presses) == 0 { // with no hook, nothing would take it
			r.Answer("")
			return
		}
		b.post(r)
	case Incoming:
		b.post(r)
	}
}

// overhear runs the OnMessage hooks for an unaddressed group message, and keeps
// the Context one returns in history, unless it is Done, which keeps nothing.
func (b *Bot) overhear(msg feature.Message) {
	b.replaced(msg)
	if a := b.onMessage(b.ctx, msg); a.Context != "" && !a.Done && b.ai != nil { // with no model, nothing would read it
		b.chats.Add(msg.Chat.Key(), b.now(), msg.Expires, chats.Entry{Role: "user", Content: a.Context, Context: true, Refs: refs(msg)})
	}
}

// ref names a message in history by its sender and when it was first sent;
// "" for one the bot made.
func ref(sender string, sent int64) string {
	if sender == "" || sent == 0 {
		return ""
	}
	return sender + "\x00" + strconv.FormatInt(sent, 10)
}

// botSender names the bot's own messages in history: no network ID can be
// it, so the core needn't know the account's.
const botSender = "\x00bot"

// pending is a ref no message has, held by an entry for what the bot is
// about to send until Send says what to call it; each is its own.
func (b *Bot) pending() chats.Ref {
	return chats.Ref{ID: botSender + "\x00pending\x00" + strconv.FormatInt(b.sends.Add(1), 10)}
}

// sendKept sends text to chat, after entries were kept holding ref p, then
// names them by what was sent, so a delete from a linked device finds them,
// and returns Send's timestamps. History is kept first so a message
// arriving while the send is out lands after the turn, not before it.
func (b *Bot) sendKept(key string, p chats.Ref, chat feature.Chat, text string, pics []feature.Picture, known map[string]string) []int64 {
	sent := b.net.Send(chat, text, pics, known, 0, nil, feature.Markup{})
	b.chats.Retag(key, p.ID, botRefs(sent))
	return sent
}

// botRefs names the messages the bot sent, by Send's timestamps.
func botRefs(sent []int64) []chats.Ref {
	var out []chats.Ref
	for _, ts := range sent {
		if id := ref(botSender, ts); id != "" {
			out = append(out, chats.Ref{ID: id, At: ts})
		}
	}
	return out
}

// version is when m was sent, or for an edit when it was edited.
func version(m feature.Message) int64 { return cmp.Or(m.Edited, m.Timestamp) }

// refs names the messages in history, for an entry that holds them: each
// message, and the one it quotes, whose text the entry holds too, so
// deleting or editing that one drops the entry.
func refs(msgs ...feature.Message) []chats.Ref {
	var out []chats.Ref
	for _, m := range msgs {
		if id := ref(m.Sender, m.Timestamp); id != "" {
			out = append(out, chats.Ref{ID: id, At: version(m)})
		}
		if q := m.Quote; q != nil {
			if id := ref(q.Sender, q.Sent); id != "" {
				out = append(out, chats.Ref{ID: id, At: q.Sent})
			}
		}
	}
	return out
}

// gone reports whether m was deleted, or replaced by a later edit, since it
// arrived.
func (b *Bot) gone(m feature.Message) bool {
	sender := m.Sender
	if m.Bot {
		sender = botSender
	}
	return b.chats.Gone(m.Chat.Key(), ref(sender, m.Timestamp), version(m))
}

// replaced forgets the versions of the message an edit replaces, in history
// and any still waiting, so history never holds two.
func (b *Bot) replaced(msg feature.Message) {
	if msg.Edited != 0 {
		b.chats.Forget(msg.Chat.Key(), ref(msg.Sender, msg.Timestamp), msg.Edited)
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
	if err := b.chats.Post(r.chat().Key(), r); err != nil {
		b.dropped(r, err)
	}
}

// errMadeRoom drops the bot's waiting reactions to make room in their
// chat's mailbox.
var errMadeRoom = errors.New("made room in its chat for a message")

// dropped logs that r was dropped for err, saying what it was, and tells a
// post's feature.
func (b *Bot) dropped(r Received, err error) {
	what := "a message"
	switch r := r.(type) {
	case Outgoing:
		what = "a post"
		r.Post.OnSent(nil, true)
	case Reacted:
		what = "a reaction"
		if n := len(r.Reactions); n > 1 {
			what = fmt.Sprintf("%d reactions", n)
		}
	case Pressed:
		what = "a press"
		r.Answer("")
	}
	if errors.Is(err, chats.ErrNoSlot) {
		logx.Warnf("dropped %s: all %d worker slots are busy (raise MAX_CONCURRENCY)", what, b.cfg.MaxConcurrency)
	} else {
		logx.Warnf("dropped %s: %v", what, err)
	}
}

// abandon tells the posts among rs, left waiting when shutdown began, that
// they were dropped, and answers the presses.
func abandon(rs []Received) {
	for _, r := range rs {
		switch r := r.(type) {
		case Outgoing:
			r.Post.OnSent(nil, true)
		case Pressed:
			r.Answer("")
		}
	}
}

// work handles what waited in a chat's mailbox, in order. Addressed
// messages are answered together, as one turn, so "actually" and "forget
// it" get one reply that sees both; each run stays within one message's
// picture limits. On its own goes a message a hook acts on (a command or
// an answer), a delivered one, a post, the bot's reactions, a press, and one that
// waited for the bot's ID and was not addressed after all. A panic loses
// only the message or turn that caused it, and nothing new starts once
// shutdown begins; a post left waiting then, or a hook's answer not yet
// sent, is told it was dropped.
func (b *Bot) work(rs []Received) {
	var batch []feature.Message
	flush := func() {
		// One deleted, or replaced by an edit, since it arrived is left
		// out, such as an original whose edit waited with it.
		run := slices.DeleteFunc(batch, b.gone)
		batch = nil
		if len(run) > 0 && !b.closing.Load() {
			safely(func() { m, text := merge(run); b.respond(m, feature.Action{}, text, refs(run...)) })
		}
	}
	alone := func(fn func()) {
		flush()
		if !b.closing.Load() {
			fn()
		}
	}
	for i, r := range rs {
		if b.closing.Load() {
			abandon(rs[i:])
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
			case Outgoing:
				alone(func() { b.say(r.Post) })
				r.Post.OnSent(nil, true) // if shutdown began first
				return
			case Reacted:
				alone(func() { b.react(r) })
				return
			case Pressed:
				alone(func() { b.pressed(r) })
				r.Answer("") // if shutdown began first
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
				if act := b.hooked(msg); !act.IsZero() {
					alone(func() { b.respond(msg, act, prompt(msg), refs(msg)) })
					if act.OnSent != nil {
						act.OnSent(nil, true) // if shutdown began first
					}
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
	if msg.Chat.GroupID == "" && msg.Edited == 0 && len(msg.Media) == 0 {
		if cmd, ok := b.keys.command(key, msg.Text); ok { // a key of the DM's keyboard
			msg.Text = cmd
		}
	}
	edit := ""
	if msg.Edited != 0 {
		edit = " (edit)"
	}
	if msg.Chat.GroupID != "" {
		logx.Infof("received group chat%s", edit)
	} else {
		logx.Infof("received dm%s", edit)
	}
	// Neither a renamed sender nor two different photos are a repeat.
	id := msg.Sender + "\x00" + msg.Name + "\x00" + msg.Text
	for _, m := range msg.Media {
		id += fmt.Sprintf("\x00%s:%t", m.ID, m.Sticker)
	}
	if b.gone(msg) {
		logx.Debugf("ignored a message deleted or edited before it was answered")
		return msg, false
	}
	if b.chats.Repeat(key, id) {
		logx.Debugf("ignored duplicate")
		return msg, false
	}
	b.replaced(msg)
	return msg, true
}

// onMessage runs the OnMessage hooks in order. The first Action that is
// non-zero, counting only what applies to the message, wins.
func (b *Bot) onMessage(ctx context.Context, msg feature.Message) feature.Action {
	for _, fn := range b.messages {
		if a := fn(ctx, msg).For(msg); !a.IsZero() {
			return a
		}
	}
	return feature.Action{}
}

// slowHooks is how long an addressed message's OnMessage hooks may run
// before the chat sees the bot typing.
const slowHooks = time.Second

// hooked runs the OnMessage hooks for an addressed message, in its chat's
// slot, and makes the OnSent of the Action that wins safe to call more than
// once, so it can be told the answer was dropped wherever it isn't sent.
// If the hooks haven't returned after slowHooks, the chat sees the bot
// typing until they do, as it would while the model answers; quick ones
// show nothing.
func (b *Bot) hooked(msg feature.Message) feature.Action {
	var mu sync.Mutex
	var stop func()
	done := false
	timer := time.AfterFunc(slowHooks, func() {
		mu.Lock()
		defer mu.Unlock()
		if !done {
			stop = b.net.Typing(msg.Chat)
		}
	})
	defer func() { // also if a hook panics
		timer.Stop()
		mu.Lock()
		defer mu.Unlock()
		done = true
		if stop != nil {
			stop()
		}
	}()
	act := b.onMessage(b.ctx, msg)
	if act.OnSent != nil {
		act.OnSent = once(act.OnSent)
	}
	return act
}

// introLine is a feature's line for the introduction, perhaps only in a DM.
type introLine struct {
	text   string
	dmOnly bool
}

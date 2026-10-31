// Package bot is the core: it routes messages from any number of chat
// networks, asks the model, and sends replies, calling the hooks features
// registered. It knows no feature, network or provider by name.
package bot

import (
	"cmp"
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/history"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/llm"
)

const (
	// maxReplyRunes caps a whole reply, so one request cannot fan out into
	// many messages.
	maxReplyRunes = 8000
	// maxQuoteRunes caps quoted text spliced into a prompt.
	maxQuoteRunes = 500
	// dedupeWindow: the same text from the same sender in the same chat
	// within this long is ignored.
	dedupeWindow = 10 * time.Second
)

// Config is the core's settings.
type Config struct {
	Model          string // as the provider names it, e.g. "deepseek/deepseek-v4.1-flash-1017"
	ExtraPrompt    string // SYSTEM_PROMPT, appended to the built-in prompt
	MaxConcurrency int
	HistoryTurns   int
	Caps           feature.Caps
	Env            feature.Env
	TypingEvery    time.Duration // how often typing is re-sent while answering; 0 for 10s
}

// Bot answers messages.
type Bot struct {
	cfg        *Config
	transports map[string]chat.Transport // by kind: the ChatID prefix
	ai         llm.Completer
	sem        chan struct{}
	ctx        context.Context
	closing    atomic.Bool // set by Shutdown: no new reply starts
	hist       *history.Store
	hooks      hooks

	mu     sync.Mutex
	dedupe map[string]time.Time

	typingEvery time.Duration
}

// New makes a bot on the given transports, asking ai.
func New(ctx context.Context, cfg *Config, ai llm.Completer, transports ...chat.Transport) *Bot {
	b := &Bot{
		cfg: cfg, ai: ai, ctx: ctx,
		transports:  map[string]chat.Transport{},
		sem:         make(chan struct{}, cfg.MaxConcurrency),
		dedupe:      map[string]time.Time{},
		typingEvery: cmp.Or(cfg.TypingEvery, 10*time.Second),
		hooks:       hooks{commands: map[string]commandFunc{}},
	}
	for _, t := range transports {
		b.transports[t.Kind()] = t
	}
	b.hist = history.New(func() int { return cfg.HistoryTurns })
	b.hist.OnForget(func(c chat.ChatID) {
		for _, fn := range b.hooks.forget {
			fn(c)
		}
		if t := b.tr(c); t != nil {
			t.Forget(c) // nor names to mention
		}
	})
	return b
}

// Run reads every transport until ctx ends.
func (b *Bot) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, t := range b.transports {
		wg.Add(1)
		go func() { defer wg.Done(); t.Run(ctx, b.Handle) }()
	}
	wg.Wait()
}

// tr is the transport that owns chat c.
func (b *Bot) tr(c chat.ChatID) chat.Transport { return b.transports[c.Transport()] }

// Shutdown stops new replies starting and waits up to d for those in flight.
// Every reply holds a concurrency slot, so once Shutdown holds them all, none
// is left.
func (b *Bot) Shutdown(d time.Duration) {
	b.closing.Store(true)
	timeout := time.After(d)
	for range cap(b.sem) {
		select {
		case b.sem <- struct{}{}:
		case <-timeout:
			logx.Warnf("shutdown timed out with replies still in flight")
			return
		}
	}
}

// Handle is called by a transport's reader goroutine. It must not block,
// and must not let a remote sender create unbounded goroutines, tickers or
// RPCs: the concurrency slot is taken before anything is spawned.
func (b *Bot) Handle(in chat.Incoming) {
	defer logx.QuietRecover("reading a message")
	if b.closing.Load() {
		return
	}
	if !in.Addressed {
		// Not for the bot, and never logged. A group message goes to the
		// OnUnaddressed hooks, if any; its Load is cheap: no network.
		if in.Group && len(b.hooks.unaddressed) > 0 {
			if m, ok := in.Load(b.ctx); ok {
				for _, fn := range b.hooks.unaddressed {
					fn(m)
				}
			}
		}
		return
	}
	select {
	case b.sem <- struct{}{}:
	default:
		logx.Warnf("at capacity (%d), dropping message", b.cfg.MaxConcurrency)
		return
	}
	go func() {
		defer func() { <-b.sem }()
		defer logx.QuietRecover("answering a message")
		if m, ok := in.Load(b.ctx); ok {
			b.handle(m)
		}
	}()
}

func (b *Bot) handle(m chat.Message) {
	req := newRequest(m)
	if b.duplicate(req) {
		logx.Debugf("ignored duplicate from %s", req.Name)
		return
	}
	if name, rest, ok := parseCommand(req.Text); ok {
		if run, ok := b.hooks.commands[name]; ok {
			if c, ok := run(b.ctx, b.turn(m, ""), rest); ok {
				if c.Tool == "" {
					b.sendText(req, c.Text)
					return
				}
				req.Tool = c.Tool
			}
		}
	}
	logx.Infof("%s", req.logLine())
	b.respond(req)
}

// sendText sends the bot's own text, not the model's, to req's chat.
func (b *Bot) sendText(req *Request, text string) {
	if err := b.tr(req.Chat).Send(b.ctx, chat.Outgoing{Chat: req.Chat, Text: text}); err != nil {
		logx.Errorf("send to %s failed: %v", req.Name, err)
	}
}

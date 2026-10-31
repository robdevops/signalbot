// Package bot is the core: it routes messages from any number of chat
// networks, asks the model, and sends replies, calling the hooks features
// registered. It knows no feature, network or provider by name.
package bot

import (
	"cmp"
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/history"
	"github.com/robdevops/signalbot/internal/kit/img"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
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

// Request is a message the bot has decided to answer.
type Request struct {
	Message chat.Message
	Chat    chat.ChatID
	Group   bool
	Key     string // history key: the chat id
	Prompt  string

	Kind    string // "mention", "reply" or "dm"
	Sender  string // display name and id, for the log
	Name    string // display name, else the id: short form for the log
	Display string // display name, "" if unknown
	Text    string // message text with the bot mention stripped

	Expires time.Duration // the chat's disappearing-messages timer, 0 if off
	Tool    string        // a chat command's tool, which the model must call
	Images  []chat.Media  // images in or quoted by the message
	Sticker *chat.Media   // a sticker sent as the message
}

// newRequest builds the prompt for an addressed message.
func newRequest(m chat.Message) *Request {
	text := m.Text
	if q := m.Quote; q != nil {
		// The sender composes the quote, so it may not be what the author
		// wrote. Escape it so it cannot close the wrapper and pose as a new
		// turn, and clamp it so it cannot flood. A message the sender tagged
		// the bot into is context they chose to share, whoever wrote it.
		quoted := strconv.Quote(txt.Clamp(q.Text, maxQuoteRunes))
		if q.FromBot {
			text = "[Replying to a message attributed to you: " + quoted + "]\n" + text
		} else {
			text = "[Replying to a message by " + strconv.Quote(cmp.Or(q.Author, "someone")) + ": " + quoted + "]\n" + text
		}
	}
	name := m.Sender.Name
	if m.Group && name != "" {
		text = name + ": " + text
	}
	who := m.Sender.ID
	if name != "" {
		who = name + "(" + m.Sender.ID + ")"
	}
	req := &Request{Message: m, Chat: m.Chat, Group: m.Group, Key: string(m.Chat), Prompt: text, Kind: m.Why, Sender: who,
		Name: cmp.Or(name, m.Sender.ID), Display: name, Text: m.Text, Expires: m.Expires}
	for _, md := range m.Media {
		switch {
		case md.Kind == chat.Sticker:
			req.Sticker = &md
		case len(req.Images) < img.MaxPerMessage:
			req.Images = append(req.Images, md)
		}
	}
	return req
}

// logLine says who sent the request, never what they said.
func (r *Request) logLine() string {
	line := r.Kind + " from " + r.Sender
	switch n := len(r.Images); n {
	case 0:
	case 1:
		line += " with 1 image"
	default:
		line += fmt.Sprintf(" with %d images", n)
	}
	if r.Sticker != nil {
		line += " with a sticker"
	}
	return line
}

// replyLogLine is the one-line summary of a model reply, e.g.
// "reply Alice 3.3s, tokens in/out: 1203/312 (10% cached), $0.002".
func replyLogLine(name string, took time.Duration, c llm.Completion) string {
	cached := 100 * min(c.CachedTokens, c.InputTokens) / max(c.InputTokens, 1)
	return fmt.Sprintf("reply %s %.1fs, tokens in/out: %d/%d (%d%% cached), $%.3f",
		name, took.Seconds(), c.InputTokens, c.OutputTokens, cached, c.Cost)
}

// parseCommand splits "/name rest" into the lowercased name and the rest.
func parseCommand(text string) (name, rest string, ok bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", "", false
	}
	name, rest, _ = strings.Cut(text[1:], " ")
	return strings.ToLower(name), strings.TrimSpace(rest), name != ""
}

// duplicate reports whether the same sender sent the same text to the same
// chat within the dedupe window, and records this one.
func (b *Bot) duplicate(req *Request) bool {
	key := req.Sender + "\x00" + req.Key + "\x00" + req.Text
	for _, a := range req.Images { // two different photos are not duplicates
		key += "\x00" + a.ID
	}
	if s := req.Sticker; s != nil {
		key += "\x00" + s.ID
	}
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, t := range b.dedupe {
		if now.Sub(t) > dedupeWindow {
			delete(b.dedupe, k)
		}
	}
	if _, ok := b.dedupe[key]; ok {
		return true
	}
	b.dedupe[key] = now
	return false
}

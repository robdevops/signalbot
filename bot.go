package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

type Bot struct {
	cfg *Config
	sig *Signal
	ai  *XAI
	sem chan struct{}

	mu      sync.Mutex
	history map[string][]ChatMsg
	names   *Names
	known   map[string]map[string]string // chat key -> name -> uuid shown to the model

	typingEvery time.Duration
}

func NewBot(cfg *Config, sig *Signal, ai *XAI) *Bot {
	return &Bot{cfg: cfg, sig: sig, ai: ai, sem: make(chan struct{}, cfg.MaxConcurrency), history: map[string][]ChatMsg{}, names: NewNames(), known: map[string]map[string]string{}, typingEvery: 10 * time.Second}
}

// Request is a message the bot has decided to answer.
type Request struct {
	Target Target
	Key    string // conversation key
	Prompt string

	Kind   string // "mention", "reply" or "dm"
	Sender string
	Text   string // message text with the bot mention stripped

	// Names the model has been shown in this message (name -> uuid), so "@Name"
	// in its reply can become a real Signal mention. Groups only.
	Names map[string]string
}

// LogLine renders the request as a single line for stdout.
func (r *Request) LogLine(now time.Time) string {
	chat := "dm"
	if r.Target.GroupID != "" {
		chat = r.Target.GroupID
	}
	text := strings.Join(strings.Fields(r.Text), " ")
	return fmt.Sprintf("%s %s from=%s chat=%s: %s", now.Format(time.RFC3339), r.Kind, r.Sender, chat, text)
}

// Decide applies the trigger rules. It returns nil if the bot should stay silent.
func Decide(env *Envelope, self string, allowed map[string]bool, names *Names) *Request {
	dm := env.DataMessage
	if dm == nil || self == "" {
		return nil
	}
	sender := env.SourceUUID
	if sender == "" {
		sender = env.Source
	}
	if sender == self || env.SourceUUID == self || env.Source == self || env.SourceNumber == self {
		return nil
	}

	var (
		target    Target
		key       string
		triggered bool
		kind      string
	)
	if dm.GroupInfo != nil && dm.GroupInfo.GroupID != "" {
		target = Target{GroupID: dm.GroupInfo.GroupID}
		key = "g:" + dm.GroupInfo.GroupID
		for _, m := range dm.Mentions {
			if m.UUID == self || (m.Number != "" && m.Number == self) {
				triggered, kind = true, "mention"
			}
		}
		if q := dm.Quote; q != nil && (q.AuthorUUID == self || q.Author == self) && !triggered {
			triggered, kind = true, "reply"
		}
	} else {
		target = Target{User: sender}
		key = "u:" + sender
		triggered = allowed[env.SourceUUID] || allowed[env.Source] || allowed[env.SourceNumber]
		kind = "dm"
	}
	if !triggered {
		return nil
	}

	text := stripMentions(dm.Message, dm.Mentions, self, names)
	if text == "" {
		return nil
	}
	raw := text
	if q := dm.Quote; q != nil && q.Text != "" && (q.AuthorUUID == self || q.Author == self) {
		text = "[In reply to your earlier message: \"" + q.Text + "\"]\n" + text
	}
	if target.GroupID != "" && env.SourceName != "" {
		text = env.SourceName + ": " + text
	}
	who := sender
	if env.SourceName != "" {
		who = env.SourceName + "(" + sender + ")"
	}
	req := &Request{Target: target, Key: key, Prompt: text, Kind: kind, Sender: who, Text: raw}
	if target.GroupID != "" {
		req.Names = map[string]string{}
		if env.SourceName != "" && env.SourceUUID != "" {
			req.Names[env.SourceName] = env.SourceUUID
		}
		if env.SourceUUID != "" {
			if n := names.Resolve(Mention{UUID: env.SourceUUID}); n != unnamedUser {
				req.Names[n] = env.SourceUUID
			}
		}
		for _, m := range dm.Mentions {
			if m.UUID == "" || m.UUID == self || m.Number == self {
				continue
			}
			if n := names.Resolve(m); n != unnamedUser {
				req.Names[n] = m.UUID
			}
		}
	}
	return req
}

// stripMentions removes the bot's own mention and expands other mentions to @name.
// Mention offsets are UTF-16 code units.
func stripMentions(msg string, ms []Mention, self string, names *Names) string {
	u := utf16.Encode([]rune(msg))
	for i := len(ms) - 1; i >= 0; i-- {
		m := ms[i]
		if m.Start < 0 || m.Length < 0 || m.Start+m.Length > len(u) {
			continue
		}
		var repl []uint16
		if m.UUID != self && m.Number != self {
			name := names.Resolve(m)
			if name != unnamedUser {
				name = "@" + name
			}
			repl = utf16.Encode([]rune(name))
		}
		u = append(u[:m.Start], append(repl, u[m.Start+m.Length:]...)...)
	}
	return strings.TrimSpace(string(utf16.Decode(u)))
}

// Handle is called by the reader; it never blocks on the model.
func (b *Bot) Handle(env *Envelope) {
	go func() {
		self := b.sig.WaitSelf(10 * time.Second)
		b.names.Learn(env)
		b.refreshIfUnknownMentions(env, self)
		req := Decide(env, self, b.cfg.AllowedSenders, b.names)
		if req == nil {
			return
		}
		b.remember(req)
		fmt.Fprintln(os.Stdout, req.LogLine(time.Now()))
		stop := b.keepTyping(req.Target)
		defer stop()
		b.sem <- struct{}{}
		defer func() { <-b.sem }()
		b.respond(context.Background(), req, stop)
	}()
}

// refreshIfUnknownMentions loads the contact list when a message mentions
// someone (other than the bot) we have no name for.
func (b *Bot) refreshIfUnknownMentions(env *Envelope, self string) {
	if env.DataMessage == nil {
		return
	}
	for _, m := range env.DataMessage.Mentions {
		if m.UUID != self && m.Number != self && b.names.needsRefresh(m) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			b.names.Refresh(ctx, b.sig)
			return
		}
	}
}

// keepTyping sends a typing indicator now and every typingEvery until stop is called.
func (b *Bot) keepTyping(t Target) (stop func()) {
	done := make(chan struct{})
	var once sync.Once
	b.sig.Typing(t)
	go func() {
		tick := time.NewTicker(b.typingEvery)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				b.sig.Typing(t)
			}
		}
	}()
	return func() { once.Do(func() { close(done) }) }
}

func (b *Bot) respond(ctx context.Context, req *Request, stopTyping func()) {
	msgs := append(b.getHistory(req.Key), ChatMsg{Role: "user", Content: req.Prompt})
	start := time.Now()
	reply, err := b.ai.Complete(ctx, msgs)
	stopTyping()
	if err != nil {
		slog.Error("x.ai request failed", "chat", req.Key, "duration", time.Since(start).Round(time.Millisecond), "err", err)
		reply = "Sorry, I couldn't get an answer right now."
	} else {
		slog.Info("x.ai reply", "chat", req.Key, "duration", time.Since(start).Round(time.Millisecond), "chars", len([]rune(reply)))
		b.addHistory(req.Key, ChatMsg{Role: "user", Content: req.Prompt}, ChatMsg{Role: "assistant", Content: reply})
	}
	parts := splitMessage(reply, 4000)
	if len(parts) == 0 {
		slog.Warn("empty reply, nothing to send", "chat", req.Key)
		return
	}
	known := b.knownNames(req.Key)
	for _, part := range parts {
		if err := b.sig.Send(req.Target, part, buildMentions(part, known)); err != nil {
			slog.Error("signal send failed", "chat", req.Key, "err", err)
			return
		}
	}
	slog.Info("reply sent to signal-cli", "chat", req.Key, "parts", len(parts))
}

// remember records names shown to the model so later replies can mention them.
func (b *Bot) remember(req *Request) {
	if len(req.Names) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	k := b.known[req.Key]
	if k == nil || len(k) > 64 {
		k = map[string]string{}
		b.known[req.Key] = k
	}
	for n, id := range req.Names {
		k[n] = id
	}
}

func (b *Bot) knownNames(key string) map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]string, len(b.known[key]))
	for n, id := range b.known[key] {
		out[n] = id
	}
	return out
}

func (b *Bot) getHistory(key string) []ChatMsg {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]ChatMsg(nil), b.history[key]...)
}

func (b *Bot) addHistory(key string, msgs ...ChatMsg) {
	b.mu.Lock()
	defer b.mu.Unlock()
	h := append(b.history[key], msgs...)
	if max := b.cfg.HistoryTurns * 2; len(h) > max {
		h = append([]ChatMsg(nil), h[len(h)-max:]...)
	}
	b.history[key] = h
}

func splitMessage(s string, max int) []string {
	var out []string
	for len([]rune(s)) > max {
		r := []rune(s)
		cut := max
		if i := strings.LastIndex(string(r[:max]), "\n"); i > 0 {
			cut = len([]rune(string(r[:max])[:i]))
		}
		out = append(out, strings.TrimSpace(string(r[:cut])))
		s = strings.TrimSpace(string(r[cut:]))
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

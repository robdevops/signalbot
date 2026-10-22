package main

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf16"
)

const (
	// signalMaxChars is Signal's per-message limit; replies are split at it.
	signalMaxChars = 2000
	// maxReplyRunes caps a whole reply, so one request cannot fan out into
	// hundreds of Signal messages.
	maxReplyRunes = 4 * signalMaxChars
	// maxNameRunes caps any display name spliced into a prompt or a log line.
	maxNameRunes = 64
	// maxQuoteRunes caps quoted text spliced into a prompt.
	maxQuoteRunes = 500
	// maxChats bounds the chats remembered: chat keys come from the network.
	maxChats = 500
	// dedupeWindow: the same text from the same sender in the same chat
	// within this long is ignored.
	dedupeWindow = 10 * time.Second
)

// safeName makes an untrusted display name safe to splice into a prompt or a
// log line: no newlines, no runs of whitespace, bounded length. Signal profile
// names are chosen by their owner, so they are attacker-controlled.
func safeName(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxNameRunes {
		s = strings.TrimSpace(string(r[:maxNameRunes]))
	}
	return s
}

func clamp(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

type Bot struct {
	cfg     *Config
	sig     *Signal
	ai      *XAI
	sem     chan struct{}
	ctx     context.Context
	closing atomic.Bool // set by Shutdown: no new reply starts

	mu      sync.Mutex
	history map[string][]ChatMsg
	known   map[string]map[string]string // chat key -> name -> uuid shown to the model
	names   *Names
	dedupe  map[string]time.Time

	typingEvery time.Duration
}

func NewBot(ctx context.Context, cfg *Config, sig *Signal, ai *XAI) *Bot {
	return &Bot{
		cfg: cfg, sig: sig, ai: ai, ctx: ctx,
		sem:         make(chan struct{}, cfg.MaxConcurrency),
		history:     map[string][]ChatMsg{},
		known:       map[string]map[string]string{},
		names:       NewNames(),
		dedupe:      map[string]time.Time{},
		typingEvery: 10 * time.Second,
	}
}

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
			logf("shutdown timed out with replies still in flight")
			return
		}
	}
}

// Request is a message the bot has decided to answer.
type Request struct {
	Target Target
	Key    string // conversation key
	Prompt string

	Kind   string // "mention", "reply" or "dm"
	Sender string // display name and id, for the log
	Name   string // display name, else the id: short form for the log
	Text   string // message text with the bot mention stripped

	// Names the model has been shown in this message (name -> uuid), so
	// "@Name" in its reply can become a real Signal mention. Groups only.
	Names map[string]string
}

// logLine says who sent the request, plus the text (quoted) when withBody.
func (r *Request) logLine(withBody bool) string {
	line := r.Kind + " from " + r.Sender
	if withBody {
		line += ": " + strconv.Quote(r.Text)
	}
	return line
}

// replyLogLine is the one-line summary of a model reply, e.g.
// "reply Rob 3.3s, tokens in/out: 1203/312 (10% cached), $0.002".
func replyLogLine(name string, took time.Duration, c Completion, cfg *Config) string {
	cached := 100 * min(c.CachedTokens, c.InputTokens) / max(c.InputTokens, 1)
	return fmt.Sprintf("reply %s %.1fs, tokens in/out: %d/%d (%d%% cached), $%.3f",
		name, took.Seconds(), c.InputTokens, c.OutputTokens, cached, c.Cost(cfg))
}

// trigger is the cheap, allocation-free half of the trigger rules: it says why
// the bot should answer ("mention", "reply" or "dm"), or "" to stay silent. It
// runs inline on the reader goroutine so that messages the bot will ignore
// cost nothing: no goroutine, no lock, no RPC.
func trigger(env *Envelope, self string, allowed map[string]bool) string {
	dm := env.DataMessage
	if dm == nil || self == "" || strings.TrimSpace(dm.Message) == "" ||
		env.SourceUUID == self || env.Source == self || env.SourceNumber == self {
		return ""
	}
	if dm.GroupInfo == nil || dm.GroupInfo.GroupID == "" {
		if allowed[env.SourceUUID] || allowed[env.Source] || allowed[env.SourceNumber] {
			return "dm"
		}
		return ""
	}
	for _, m := range dm.Mentions {
		if m.UUID == self || m.Number == self {
			return "mention"
		}
	}
	if q := dm.Quote; q != nil && (q.AuthorUUID == self || q.Author == self) {
		return "reply"
	}
	return ""
}

// Decide builds the prompt for a message that triggered as kind. It returns
// nil if nothing is left once the bot's own mention is stripped.
func Decide(env *Envelope, kind, self string, names *Names) *Request {
	dm := env.DataMessage
	sender := cmp.Or(env.SourceUUID, env.Source)
	target, key := Target{User: sender}, "u:"+sender
	if kind != "dm" {
		target, key = Target{GroupID: dm.GroupInfo.GroupID}, "g:"+dm.GroupInfo.GroupID
	}

	text := stripMentions(dm.Message, dm.Mentions, self, names)
	if text == "" {
		return nil
	}
	raw := text
	if q := dm.Quote; q != nil && q.Text != "" && (q.AuthorUUID == self || q.Author == self) {
		// The sender composes the quote, so it may not be what the bot said.
		// Escape it so it cannot close the wrapper and pose as a new turn, and
		// clamp it so it cannot flood.
		text = "[Replying to a message attributed to you: " + strconv.Quote(clamp(q.Text, maxQuoteRunes)) + "]\n" + text
	}
	name := safeName(env.SourceName)
	if target.GroupID != "" && name != "" {
		text = name + ": " + text
	}
	who := sender
	if name != "" {
		who = name + "(" + sender + ")"
	}

	req := &Request{Target: target, Key: key, Prompt: text, Kind: kind, Sender: who, Name: cmp.Or(name, sender), Text: raw}
	if target.GroupID != "" {
		req.Names = map[string]string{}
		add := func(n, uuid string) {
			if n = safeName(n); n != "" && uuid != "" && uuid != self {
				bindName(req.Names, n, uuid)
			}
		}
		add(env.SourceName, env.SourceUUID) // the name the model sees in the prompt
		for _, m := range dm.Mentions {
			if m.Number != self {
				add(names.Resolve(m), m.UUID)
			}
		}
	}
	return req
}

// stripMentions removes the bot's own mention and expands other mentions to
// @name. Offsets are UTF-16 code units. Mentions are applied from the end so
// earlier offsets stay valid, which requires sorting: signal-cli's order is
// not guaranteed, and an out-of-order pair would splice over live text.
func stripMentions(msg string, ms []Mention, self string, names *Names) string {
	u := utf16.Encode([]rune(msg))
	sorted := append([]Mention(nil), ms...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Start > sorted[j].Start })

	applied := len(u) // lowest offset already rewritten
	for _, m := range sorted {
		// Written so that no pair of in-range int values can overflow.
		if m.Start < 0 || m.Length < 0 || m.Start > len(u) || m.Length > len(u)-m.Start {
			continue
		}
		if m.Start+m.Length > applied { // overlaps one we already applied
			continue
		}
		var repl []uint16
		if m.UUID != self && m.Number != self {
			name := "@" + names.Resolve(m)
			if name == "@" {
				logf("unresolved mention of %s", cmp.Or(m.UUID, m.Number))
				name = unnamedUser
			}
			repl = utf16.Encode([]rune(name))
		}
		u = append(u[:m.Start], append(repl, u[m.Start+m.Length:]...)...)
		applied = m.Start
	}
	return strings.TrimSpace(string(utf16.Decode(u)))
}

// Handle is called by the reader goroutine. It must not block, and must not
// let a remote sender create unbounded goroutines, tickers or RPCs: the
// concurrency slot is taken before anything is spawned.
func (b *Bot) Handle(env *Envelope) {
	if env.DataMessage == nil || b.closing.Load() {
		return
	}
	// Cheap inline rejection. Before the account id is known (the first moment
	// after connecting) we cannot judge, so we let it through to the slow path.
	if self := b.sig.SelfID(); self != "" && trigger(env, self, b.cfg.AllowedSenders) == "" {
		// Remember group members' names, so they resolve if mentioned later.
		// It is cheap and bounded, and only group senders: they are members of
		// a group the bot is in. DMs can come from anyone and are ignored.
		if g := env.DataMessage.GroupInfo; g != nil && g.GroupID != "" {
			b.names.Learn(env)
		}
		return
	}
	select {
	case b.sem <- struct{}{}:
	default:
		logf("at capacity (%d), dropping message", b.cfg.MaxConcurrency)
		return
	}
	go func() {
		defer func() { <-b.sem }()
		b.handle(env)
	}()
}

func (b *Bot) handle(env *Envelope) {
	self := b.sig.WaitSelf(10 * time.Second)
	kind := trigger(env, self, b.cfg.AllowedSenders)
	if kind == "" {
		return
	}
	b.names.Learn(env)
	// Load the contact list if someone other than the bot is mentioned whose
	// name we lack. Only messages the bot will answer get this far.
	if slices.ContainsFunc(env.DataMessage.Mentions, func(m Mention) bool {
		return m.UUID != self && m.Number != self && b.names.needsRefresh(m)
	}) {
		b.refreshContacts()
	}

	req := Decide(env, kind, self, b.names)
	if req == nil {
		return
	}
	if b.duplicate(req) {
		logf("ignored duplicate from %s", req.Name)
		return
	}
	b.remember(req)
	logf("%s", req.logLine(b.cfg.LogMessageBody))
	b.respond(req)
}

// duplicate reports whether the same sender sent the same text to the same
// chat within the dedupe window, and records this one.
func (b *Bot) duplicate(req *Request) bool {
	window := dedupeWindow
	key := req.Sender + "\x00" + req.Key + "\x00" + req.Text
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, t := range b.dedupe {
		if now.Sub(t) > window {
			delete(b.dedupe, k)
		}
	}
	if _, ok := b.dedupe[key]; ok {
		return true
	}
	b.dedupe[key] = now
	return false
}

// refreshContacts loads the contact list unless that was done moments ago,
// and reports whether it tried.
func (b *Bot) refreshContacts() bool {
	ctx, cancel := context.WithTimeout(b.ctx, 10*time.Second)
	defer cancel()
	return b.names.Refresh(ctx, b.sig)
}

// keepTyping sends a typing indicator now and every typingEvery until stopped.
func (b *Bot) keepTyping(t Target) (stop func()) {
	done := make(chan struct{})
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
	return func() { close(done) }
}

func (b *Bot) respond(req *Request) {
	stopTyping := b.keepTyping(req.Target)
	b.ensureSelfName()
	msgs := append(b.getHistory(req.Key), ChatMsg{Role: "user", Content: req.Prompt})

	start := time.Now()
	res, err := b.ai.Complete(b.ctx, b.systemPrompt(), msgs)
	stopTyping()
	took := time.Since(start)
	if err != nil {
		logf("x.ai error for %s after %.1fs: %v", req.Name, took.Seconds(), err)
		res = Completion{Text: "Sorry, I couldn't get an answer right now."}
	} else {
		logf("%s", replyLogLine(req.Name, took, res, b.cfg))
		b.addHistory(req.Key, ChatMsg{Role: "user", Content: req.Prompt}, ChatMsg{Role: "assistant", Content: res.Text})
	}

	if n := len([]rune(res.Text)); n > maxReplyRunes {
		logf("reply to %s truncated from %d chars", req.Name, n)
	}
	parts := replyParts(res.Text, b.introName())
	if len(parts) == 0 {
		logf("empty reply to %s, nothing sent", req.Name)
		return
	}
	known := b.knownNames(req.Key)
	for _, part := range parts {
		if err := b.sig.Send(req.Target, part, buildMentions(part, known)); err != nil {
			logf("send to %s failed: %v", req.Name, err)
			return
		}
	}
}

const truncatedMarker = " […truncated]"

// replyParts turns a model reply into the Signal messages to send: the bot's
// own @name stripped, the whole reply capped at maxReplyRunes (so at most
// maxReplyRunes/signalMaxChars messages), then split at Signal's limit.
func replyParts(reply, self string) []string {
	reply = stripSelfTags(reply, self)
	if r := []rune(reply); len(r) > maxReplyRunes {
		reply = string(r[:maxReplyRunes-len([]rune(truncatedMarker))]) + truncatedMarker
	}
	return splitMessage(reply, signalMaxChars)
}

// stripSelfTags removes the bot's own @name from a reply, without leaving a
// double space behind.
func stripSelfTags(reply, self string) string {
	rs := []rune(reply)
	spans := findAtNames(rs, map[string]string{safeName(self): "self"})
	if len(spans) == 0 {
		return reply
	}
	var out []rune
	pos := 0
	for _, s := range spans {
		out = append(out, rs[pos:s.start]...)
		pos = s.end
		if pos < len(rs) && rs[pos] == ' ' && (len(out) == 0 || out[len(out)-1] == ' ') {
			pos++
		}
	}
	out = append(out, rs[pos:]...)
	return strings.TrimSpace(string(out))
}

const readmeURL = "https://github.com/robdevops/signalbot/blob/main/README.md"

// defaultIntro is used when INTRO is unset. {name} and {model} are substituted.
const defaultIntro = "hi, I'm {name}, your open source assistant running {model}. " +
	"I respond to @mentions and replies to my messages. See " + readmeURL + " for details"

// basePrompt always applies; SYSTEM_PROMPT is appended to it.
const basePrompt = "You are a helpful assistant in a Signal chat. " +
	"Reply in plain text: Signal does not render markdown. " +
	"Aim for a single line. Two or three sentences at the very most, and only when asked for detail. " +
	"Never exceed 2000 characters, which is Signal's limit for one message. " +
	"A mention shown as [unnamed user] is someone whose name you don't know; don't invent one. " +
	"To mention someone, write @ followed by their exact name. Never write your own name with an @."

// selfName is the bot's own profile name, or "" until contacts are loaded.
func (b *Bot) selfName() string {
	return b.names.lookup(b.sig.SelfID()).name
}

// ensureSelfName loads contacts if the bot's own name is not known yet.
func (b *Bot) ensureSelfName() {
	if b.selfName() == "" && b.refreshContacts() && b.selfName() == "" {
		logf("own profile name not found; calling itself signalbot (INTRO can name it)")
	}
}

// introName is the name the model is told it has.
func (b *Bot) introName() string { return cmp.Or(b.selfName(), "signalbot") }

// systemPrompt is the built-in prompt, plus SYSTEM_PROMPT, plus the intro.
func (b *Bot) systemPrompt() string {
	intro := strings.NewReplacer("{name}", safeName(b.introName()), "{model}", b.cfg.Model).Replace(b.cfg.Intro)
	p := basePrompt
	if b.cfg.ExtraPrompt != "" {
		p += "\n" + b.cfg.ExtraPrompt
	}
	// Quote so a newline or quote in the name or in INTRO cannot escape it.
	return p + "\nIf asked to introduce yourself, reply with exactly this text and nothing else: " + strconv.Quote(intro)
}

// remember records names shown to the model so later replies can mention them.
func (b *Bot) remember(req *Request) {
	if len(req.Names) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	makeRoom(b.known, req.Key, maxChats)
	k := b.known[req.Key]
	if k == nil {
		k = map[string]string{}
		b.known[req.Key] = k
	}
	for n, uuid := range req.Names {
		if _, ok := k[n]; ok || len(k) < 256 {
			bindName(k, n, uuid)
		}
	}
}

// bindName records that name n refers to uuid. Display names are self-chosen,
// so anyone can claim a member's name: a name claimed by two different people
// is ambiguous, marked "", and never linked.
func bindName(m map[string]string, n, uuid string) {
	n = strings.ToLower(n) // mentions match case-insensitively, so "rob" claims "Rob"
	if old, ok := m[n]; !ok {
		m[n] = uuid
	} else if old != uuid {
		m[n] = ""
	}
}

func (b *Bot) knownNames(key string) map[string]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return maps.Clone(b.known[key])
}

func (b *Bot) getHistory(key string) []ChatMsg {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]ChatMsg(nil), b.history[key]...)
}

func (b *Bot) addHistory(key string, msgs ...ChatMsg) {
	b.mu.Lock()
	defer b.mu.Unlock()
	makeRoom(b.history, key, maxChats)
	h := append(b.history[key], msgs...)
	if max := b.cfg.HistoryTurns * 2; len(h) > max {
		h = append([]ChatMsg(nil), h[len(h)-max:]...)
	}
	b.history[key] = h
}

// makeRoom makes room for key in m, dropping an arbitrary entry once m holds
// max. Keys come from the network, so these maps must be bounded. The caller
// holds the lock that guards m.
func makeRoom[V any](m map[string]V, key string, max int) {
	if _, ok := m[key]; ok || len(m) < max {
		return
	}
	for k := range m {
		delete(m, k)
		return
	}
}

// splitMessage breaks a reply into chunks of at most max runes, preferring a
// newline and then a space as the break.
func splitMessage(s string, max int) []string {
	r := []rune(s)
	var out []string
	for {
		for len(r) > 0 && unicode.IsSpace(r[0]) {
			r = r[1:]
		}
		if len(r) == 0 {
			return out
		}
		end := min(len(r), max)
		if end < len(r) {
			if i := lastIndexRune(r[:end], '\n'); i > 0 {
				end = i
			} else if i := lastIndexRune(r[:end], ' '); i > max/2 {
				end = i
			}
		}
		out = append(out, strings.TrimRightFunc(string(r[:end]), unicode.IsSpace))
		r = r[end:]
	}
}

func lastIndexRune(r []rune, want rune) int {
	for i := len(r) - 1; i >= 0; i-- {
		if r[i] == want {
			return i
		}
	}
	return -1
}

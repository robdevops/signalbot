package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	cfg *Config
	sig *Signal
	ai  *XAI
	sem chan struct{}
	ctx context.Context

	life    sync.Mutex // orders closing against wg.Add
	closing bool
	wg      sync.WaitGroup

	mu      sync.Mutex
	history map[string][]ChatMsg
	known   map[string]map[string]knownName // chat key -> name -> who to mention
	used    map[string]time.Time            // chat key -> last use, for eviction
	names   *Names
	dedupe  map[string]time.Time
	sentTS  map[int64]time.Time // timestamps of messages we sent

	typingEvery time.Duration
}

// knownName is a name the model has been shown, and who it refers to.
type knownName struct {
	uuid string
	rank int
}

func NewBot(cfg *Config, sig *Signal, ai *XAI) *Bot {
	b := &Bot{
		cfg: cfg, sig: sig, ai: ai,
		sem:         make(chan struct{}, cfg.MaxConcurrency),
		ctx:         context.Background(),
		history:     map[string][]ChatMsg{},
		known:       map[string]map[string]knownName{},
		used:        map[string]time.Time{},
		names:       NewNames(),
		dedupe:      map[string]time.Time{},
		sentTS:      map[int64]time.Time{},
		typingEvery: 10 * time.Second,
	}
	if sig != nil {
		sig.OnSent = b.recordSent
	}
	return b
}

// Close stops the bot accepting new messages. Replies already in flight
// carry on; use Wait to let them finish.
func (b *Bot) Close() {
	b.life.Lock()
	b.closing = true
	b.life.Unlock()
}

// Wait blocks until in-flight replies finish, or until d elapses.
func (b *Bot) Wait(d time.Duration) {
	done := make(chan struct{})
	go func() { b.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		logf("shutdown timed out with replies still in flight")
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

	// Names the model has been shown in this message, so "@Name" in its reply
	// can become a real Signal mention. Groups only.
	Names map[string]knownName
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
	cached := 0
	if c.InputTokens > 0 {
		cached = 100 * min(c.CachedTokens, c.InputTokens) / c.InputTokens
	}
	return fmt.Sprintf("reply %s %.1fs, tokens in/out: %d/%d (%d%% cached), $%.3f",
		name, took.Seconds(), c.InputTokens, c.OutputTokens, cached, c.Cost(cfg))
}

// triggers is the cheap, allocation-free half of the trigger rules. It runs
// inline on the reader goroutine so that messages the bot will ignore cost
// nothing: no goroutine, no lock, no RPC.
func triggers(env *Envelope, self string, allowed map[string]bool) bool {
	dm := env.DataMessage
	if dm == nil || self == "" || strings.TrimSpace(dm.Message) == "" {
		return false
	}
	if env.SourceUUID == self || env.Source == self || env.SourceNumber == self {
		return false
	}
	if dm.GroupInfo != nil && dm.GroupInfo.GroupID != "" {
		for _, m := range dm.Mentions {
			if m.UUID == self || (m.Number != "" && m.Number == self) {
				return true
			}
		}
		q := dm.Quote
		return q != nil && (q.AuthorUUID == self || q.Author == self)
	}
	return allowed[env.SourceUUID] || allowed[env.Source] || allowed[env.SourceNumber]
}

// decideCtx is everything Decide needs beyond the envelope.
type decideCtx struct {
	self    string
	allowed map[string]bool
	names   *Names
	// sent reports whether we actually sent the message with this timestamp.
	// Quote fields are composed by the sending client, so a quote attributed
	// to us is only evidence when it matches something we really sent.
	sent func(int64) bool
}

// Decide applies the trigger rules and builds the prompt. It returns nil if
// the bot should stay silent.
func Decide(env *Envelope, d decideCtx) *Request {
	if !triggers(env, d.self, d.allowed) {
		return nil
	}
	dm := env.DataMessage
	sender := env.SourceUUID
	if sender == "" {
		sender = env.Source
	}

	var target Target
	var key, kind string
	if dm.GroupInfo != nil && dm.GroupInfo.GroupID != "" {
		target = Target{GroupID: dm.GroupInfo.GroupID}
		key = "g:" + dm.GroupInfo.GroupID
		kind = "reply"
		for _, m := range dm.Mentions {
			if m.UUID == d.self || (m.Number != "" && m.Number == d.self) {
				kind = "mention"
				break
			}
		}
	} else {
		target = Target{User: sender}
		key = "u:" + sender
		kind = "dm"
	}

	text := stripMentions(dm.Message, dm.Mentions, d.self, d.names)
	if text == "" {
		return nil
	}
	raw := text
	if q := dm.Quote; q != nil && q.Text != "" && (q.AuthorUUID == d.self || q.Author == d.self) {
		label := "a message the sender claims you sent"
		if d.sent != nil && d.sent(q.ID) {
			label = "your earlier message"
		}
		// Quoted text is attacker-controlled: escape it so it cannot close the
		// wrapper and pose as a new turn, and clamp it so it cannot flood.
		text = "[In reply to " + label + ": " + strconv.Quote(clamp(q.Text, maxQuoteRunes)) + "]\n" + text
	}
	name := safeName(env.SourceName)
	if target.GroupID != "" && name != "" {
		text = name + ": " + text
	}
	who := sender
	if name != "" {
		who = name + "(" + sender + ")"
	}

	short := name
	if short == "" {
		short = sender
	}
	req := &Request{Target: target, Key: key, Prompt: text, Kind: kind, Sender: who, Name: short, Text: raw}
	if target.GroupID != "" {
		req.Names = map[string]knownName{}
		add := func(n string, rank int, uuid string) {
			if n = safeName(n); n != "" && uuid != "" && uuid != d.self {
				bindName(req.Names, n, knownName{uuid, rank})
			}
		}
		if env.SourceUUID != "" {
			add(env.SourceName, rankSeen, env.SourceUUID)
			if n, rank := d.names.resolve(Mention{UUID: env.SourceUUID, Number: env.SourceNumber}); rank > 0 {
				add(n, rank, env.SourceUUID)
			}
		}
		for _, m := range dm.Mentions {
			if m.UUID == "" || m.UUID == d.self || m.Number == d.self {
				continue
			}
			// stripMentions has already logged any miss for this mention.
			if n, rank := d.names.resolve(m); rank > 0 {
				add(n, rank, m.UUID)
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
			name := names.Resolve(m)
			if name != unnamedUser {
				name = "@" + name
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
	if env.DataMessage == nil {
		return
	}
	// Cheap inline rejection. Before the account id is known (the first moment
	// after connecting) we cannot judge, so we let it through to the slow path.
	if self := b.sig.SelfID(); self != "" && !triggers(env, self, b.cfg.AllowedSenders) {
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
	// Checked and counted under one lock, so no reply can start after Close
	// has returned and Wait is draining.
	b.life.Lock()
	if b.closing {
		b.life.Unlock()
		<-b.sem
		return
	}
	b.wg.Add(1)
	b.life.Unlock()
	go func() {
		defer b.wg.Done()
		defer func() { <-b.sem }()
		b.handle(env)
	}()
}

func (b *Bot) handle(env *Envelope) {
	self := b.sig.WaitSelf(10 * time.Second)
	if !triggers(env, self, b.cfg.AllowedSenders) {
		return
	}
	b.names.Learn(env)
	b.refreshIfUnknownMentions(env, self)

	req := Decide(env, decideCtx{self: self, allowed: b.cfg.AllowedSenders, names: b.names, sent: b.wasSent})
	if req == nil {
		return
	}
	if b.duplicate(req) {
		logf("ignored duplicate from %s", req.Name)
		return
	}
	b.remember(req)
	logf("%s", req.logLine(b.cfg.LogMessageBody))

	stop := b.keepTyping(req.Target)
	defer stop()
	b.respond(b.ctx, req, stop)
}

// duplicate reports whether the same sender sent the same text to the same
// chat within the dedupe window, and records this one.
func (b *Bot) duplicate(req *Request) bool {
	window := time.Duration(b.cfg.DedupeWindow) * time.Second
	if window <= 0 {
		return false
	}
	key := req.Sender + "\x00" + req.Key + "\x00" + req.Text
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, t := range b.dedupe {
		if now.Sub(t) > window {
			delete(b.dedupe, k)
		}
	}
	if t, ok := b.dedupe[key]; ok && now.Sub(t) <= window {
		return true
	}
	b.dedupe[key] = now
	return false
}

func (b *Bot) recordSent(ts int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cutoff := time.Now().Add(-24 * time.Hour)
	for k, t := range b.sentTS {
		if t.Before(cutoff) || len(b.sentTS) > 1000 {
			delete(b.sentTS, k)
		}
	}
	b.sentTS[ts] = time.Now()
}

func (b *Bot) wasSent(ts int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.sentTS[ts]
	return ok
}

// refreshIfUnknownMentions loads the contact list when a message mentions
// someone (other than the bot) we have no name for. It runs only for messages
// the bot has already decided to answer.
func (b *Bot) refreshIfUnknownMentions(env *Envelope, self string) {
	if env.DataMessage == nil {
		return
	}
	for _, m := range env.DataMessage.Mentions {
		if m.UUID != self && m.Number != self && b.names.needsRefresh(m) {
			b.refreshContacts()
			return
		}
	}
}

func (b *Bot) refreshContacts() {
	ctx, cancel := context.WithTimeout(b.ctx, 10*time.Second)
	defer cancel()
	b.names.Refresh(ctx, b.sig)
}

// keepTyping sends a typing indicator now and every typingEvery until stopped.
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
	b.ensureSelfName(ctx)
	msgs := append(b.getHistory(req.Key), ChatMsg{Role: "user", Content: req.Prompt})

	start := time.Now()
	res, err := b.ai.Complete(ctx, b.systemPrompt(), msgs)
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

// stripSelfTags removes the bot's own @name from a reply. Left in, it would
// render as a mention of the bot, which wakes the bot with its own message.
func stripSelfTags(reply, self string) string {
	self = safeName(self)
	if self == "" {
		return reply
	}
	rs := []rune(reply)
	spans := findAtNames(rs, map[string]knownName{self: {uuid: "self"}})
	if len(spans) == 0 {
		return reply
	}
	var out []rune
	pos := 0
	for _, s := range spans {
		out = append(out, rs[pos:s.start]...)
		pos = s.end
		before := len(out) == 0 || unicode.IsSpace(out[len(out)-1])
		if before && pos < len(rs) && (rs[pos] == ',' || rs[pos] == ':') {
			pos++ // "@bot, hi" -> "hi"
		}
		switch {
		case before && pos < len(rs) && rs[pos] == ' ':
			pos++ // "hi @bot there" -> "hi there"
		case len(out) > 0 && out[len(out)-1] == ' ' && (pos == len(rs) || unicode.IsPunct(rs[pos])):
			out = out[:len(out)-1] // "thanks @bot!" -> "thanks!"
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

// selfName is the bot's own display name: BOT_NAME, else its profile name.
func (b *Bot) selfName() string {
	if b.cfg.BotName != "" {
		return b.cfg.BotName
	}
	if r := b.names.lookup(b.sig.SelfIDs()...); r.rank > 0 {
		return r.name
	}
	return ""
}

// ensureSelfName loads contacts if the bot's own name is not known yet. The
// attempt is only counted when a lookup actually happened, so throttled
// no-op refreshes cannot exhaust the budget.
func (b *Bot) ensureSelfName(ctx context.Context) {
	if b.selfName() != "" || !b.names.canRefresh() {
		return
	}
	b.refreshContacts()
	if b.selfName() == "" {
		logf("own profile name not found; set BOT_NAME to override")
	}
}

// introName is the name the model is told it has.
func (b *Bot) introName() string {
	if name := b.selfName(); name != "" {
		return name
	}
	return "signalbot"
}

// systemPrompt is the built-in prompt, plus SYSTEM_PROMPT, plus the intro.
func (b *Bot) systemPrompt() string {
	name := b.introName()
	intro := b.cfg.Intro
	if intro == "" {
		intro = defaultIntro
	}
	intro = strings.NewReplacer("{name}", safeName(name), "{model}", b.cfg.Model).Replace(intro)

	p := basePrompt
	if extra := strings.TrimSpace(b.cfg.ExtraPrompt); extra != "" {
		p += "\n" + extra
	}
	// Quote so a newline or quote in the name or in INTRO cannot escape it.
	return p + "\nIf asked to introduce yourself, reply with exactly this text and nothing else: " + strconv.Quote(intro)
}

// bindName records that name n refers to v.uuid. Display names are chosen by
// their owners, so anyone can claim anyone's name: a name claimed by two
// different people becomes ambiguous (no uuid) and is never turned into a
// mention. The one exception is the account owner's own contact name, which
// is trusted and wins over any self-chosen claim. Reports a new conflict.
func bindName(m map[string]knownName, n string, v knownName) (conflict bool) {
	old, ok := m[n]
	switch {
	case !ok:
		m[n] = v
	case old.uuid == v.uuid:
		if v.rank > old.rank {
			m[n] = v
		}
	case v.rank >= rankContact && old.rank < rankContact:
		m[n] = v
	case old.rank >= rankContact:
		// keep the trusted binding
	default:
		m[n] = knownName{rank: max(old.rank, v.rank)}
		return old.uuid != ""
	}
	return false
}

// remember records names shown to the model so later replies can mention them.
func (b *Bot) remember(req *Request) {
	if len(req.Names) == 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.touch(req.Key)
	k := b.known[req.Key]
	if k == nil {
		k = map[string]knownName{}
		b.known[req.Key] = k
	}
	for n, v := range req.Names {
		if _, ok := k[n]; !ok && len(k) >= 256 {
			continue
		}
		if bindName(k, n, v) {
			logf("name %q is claimed by more than one user; not linking it", n)
		}
	}
}

func (b *Bot) knownNames(key string) map[string]knownName {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(map[string]knownName, len(b.known[key]))
	for n, v := range b.known[key] {
		out[n] = v
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
	b.touch(key)
	h := append(b.history[key], msgs...)
	if max := b.cfg.HistoryTurns * 2; len(h) > max {
		h = append([]ChatMsg(nil), h[len(h)-max:]...)
	}
	b.history[key] = h
}

// touch records a chat as used and evicts the least recently used chats once
// there are more than MaxChats. Chat keys come from the envelope, so without
// this a sender could grow these maps forever by creating groups.
// Caller holds b.mu.
func (b *Bot) touch(key string) {
	b.used[key] = time.Now()
	if b.cfg.MaxChats <= 0 {
		return
	}
	for len(b.used) > b.cfg.MaxChats {
		oldest, at := "", time.Time{}
		for k, t := range b.used {
			if k != key && (oldest == "" || t.Before(at)) {
				oldest, at = k, t
			}
		}
		if oldest == "" {
			return
		}
		delete(b.used, oldest)
		delete(b.history, oldest)
		delete(b.known, oldest)
	}
}

// splitMessage breaks a reply into chunks of at most max runes, preferring a
// newline and then a space as the break. It is linear: the text is converted
// to runes once and then sliced.
func splitMessage(s string, max int) []string {
	r := []rune(s)
	var out []string
	for len(r) > 0 {
		for len(r) > 0 && unicode.IsSpace(r[0]) {
			r = r[1:]
		}
		if len(r) == 0 {
			break
		}
		end := len(r)
		if end > max {
			end = max
			if i := lastIndexRune(r[:max], '\n'); i > 0 {
				end = i
			} else if i := lastIndexRune(r[:max], ' '); i > max/2 {
				end = i
			}
		}
		part := r[:end]
		for len(part) > 0 && unicode.IsSpace(part[len(part)-1]) {
			part = part[:len(part)-1]
		}
		if len(part) > 0 {
			out = append(out, string(part))
		}
		r = r[end:]
	}
	return out
}

func lastIndexRune(r []rune, want rune) int {
	for i := len(r) - 1; i >= 0; i-- {
		if r[i] == want {
			return i
		}
	}
	return -1
}

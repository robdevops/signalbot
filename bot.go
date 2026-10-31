package main

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"regexp"
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
	ai      Completer
	sem     chan struct{}
	ctx     context.Context
	closing atomic.Bool // set by Shutdown: no new reply starts

	refreshPending atomic.Bool // a contact reload is already scheduled

	mu      sync.Mutex
	history map[string][]ChatMsg
	known   map[string]map[string]string // chat key -> name -> uuid shown to the model
	names   *Names
	dedupe  map[string]time.Time

	typingEvery time.Duration
	toolNames   []string // MCP tools the model may call, for {tools}
	toolGroups  string   // the same, grouped by server, for the system prompt
	vision      bool     // the model accepts images
	toolCalls   bool     // the model can call tools (web search, charts, MCP)
}

func NewBot(ctx context.Context, cfg *Config, sig *Signal, ai Completer) *Bot {
	b := &Bot{
		cfg: cfg, sig: sig, ai: ai, ctx: ctx,
		sem:         make(chan struct{}, cfg.MaxConcurrency),
		history:     map[string][]ChatMsg{},
		known:       map[string]map[string]string{},
		names:       NewNames(),
		dedupe:      map[string]time.Time{},
		typingEvery: 10 * time.Second,
		toolCalls:   true,
	}
	sig.onConnect = b.refreshSoon // load names on startup and after every reconnect
	return b
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
			warnf("shutdown timed out with replies still in flight")
			return
		}
	}
}

// Request is a message the bot has decided to answer.
type Request struct {
	Target Target
	Key    string // conversation key
	Prompt string

	Kind    string // "mention", "reply" or "dm"
	Sender  string // display name and id, for the log
	Name    string // display name, else the id: short form for the log
	Display string // display name, "" if unknown
	Text    string // message text with the bot mention stripped

	Expires time.Duration // the chat's disappearing-messages timer, 0 if off
	Images  []Attachment  // images in or quoted by the message, fetched when answering
	Sticker *Sticker      // a sticker sent as the message, fetched when answering

	// Names the model has been shown in this message (name -> uuid), so
	// "@Name" in its reply can become a real Signal mention. Groups only.
	Names map[string]string
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
func replyLogLine(name string, took time.Duration, c Completion) string {
	cached := 100 * min(c.CachedTokens, c.InputTokens) / max(c.InputTokens, 1)
	return fmt.Sprintf("reply %s %.1fs, tokens in/out: %d/%d (%d%% cached), $%.3f",
		name, took.Seconds(), c.InputTokens, c.OutputTokens, cached, c.Cost)
}

func fromSelf(env *Envelope, self string) bool {
	return env.SourceUUID == self || env.Source == self || env.SourceNumber == self
}

func isAllowed(env *Envelope, allowed map[string]bool) bool {
	return allowed[env.SourceUUID] || allowed[env.Source] || allowed[env.SourceNumber]
}

// trigger is the cheap, allocation-free half of the trigger rules: it says why
// the bot should answer ("mention", "reply" or "dm"), or "" to stay silent. It
// runs inline on the reader goroutine so that messages the bot will ignore
// cost nothing: no goroutine, no lock, no RPC.
func trigger(env *Envelope, self string, allowed map[string]bool) string {
	dm := env.DataMessage
	if dm == nil || self == "" || fromSelf(env, self) {
		return ""
	}
	// A photo or sticker alone (no text) counts too: in a DM, or as a reply
	// to the bot.
	if strings.TrimSpace(dm.Message) == "" && dm.Sticker == nil &&
		!slices.ContainsFunc(dm.Attachments, func(a Attachment) bool { return a.image() != nil }) {
		return ""
	}
	if dm.GroupInfo == nil || dm.GroupInfo.GroupID == "" {
		if isAllowed(env, allowed) {
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
	// Images the model may see: the message's own, then the quoted message's
	// (a thumbnail), at most maxReplyImages.
	var images []Attachment
	attached := dm.Attachments
	if dm.Quote != nil {
		attached = append(slices.Clone(attached), dm.Quote.Attachments...)
	}
	for _, a := range attached {
		if img := a.image(); img != nil && len(images) < maxReplyImages {
			images = append(images, *img)
		}
	}
	if text == "" && len(images) == 0 && dm.Sticker == nil {
		return nil
	}
	raw := text
	if q := dm.Quote; q != nil && q.Text != "" {
		// The sender composes the quote, so it may not be what the author
		// wrote. Escape it so it cannot close the wrapper and pose as a new
		// turn, and clamp it so it cannot flood. A message the sender tagged
		// the bot into is context they chose to share, whoever wrote it.
		quoted := strconv.Quote(clamp(q.Text, maxQuoteRunes))
		if q.AuthorUUID == self || q.Author == self {
			text = "[Replying to a message attributed to you: " + quoted + "]\n" + text
		} else {
			author := cmp.Or(names.lookup(q.AuthorUUID, q.Author).name, "someone")
			text = "[Replying to a message by " + strconv.Quote(author) + ": " + quoted + "]\n" + text
		}
	}
	name := safeName(env.SourceName)
	if target.GroupID != "" && name != "" {
		text = name + ": " + text
	}
	who := sender
	if name != "" {
		who = name + "(" + sender + ")"
	}

	req := &Request{Target: target, Key: key, Prompt: text, Kind: kind, Sender: who, Name: cmp.Or(name, sender), Display: name, Text: raw,
		Expires: time.Duration(dm.ExpiresInSeconds) * time.Second, Images: images, Sticker: dm.Sticker}
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
				debugf("unresolved mention of %s", cmp.Or(m.UUID, m.Number))
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
	defer quietRecover("reading a message")
	if env.DataMessage == nil || b.closing.Load() {
		return
	}
	// A group changed (members, name or description): reload the contact list
	// so new members are known and departed ones are forgotten.
	if g := env.DataMessage.GroupInfo; g != nil && g.Type == "UPDATE" {
		b.forgetKnown("g:" + g.GroupID)
		b.refreshSoon()
	}
	// Cheap inline rejection. Before the account id is known (the first moment
	// after connecting) we cannot judge, so we let it through to the slow path.
	if self := b.sig.SelfID(); self != "" && trigger(env, self, b.cfg.AllowedSenders) == "" {
		// Dropped untouched: nothing is read from it or kept. The one thing
		// said is that a DM came from someone who may not use the bot, by id
		// only, never the name or text.
		if g := env.DataMessage.GroupInfo; (g == nil || g.GroupID == "") && !fromSelf(env, self) && !isAllowed(env, b.cfg.AllowedSenders) {
			infof("%s ignored because not in SIGNAL_DM_USER_IDS",
				cmp.Or(env.SourceUUID, env.Source, env.SourceNumber, "unknown sender"))
		}
		return
	}
	select {
	case b.sem <- struct{}{}:
	default:
		warnf("at capacity (%d), dropping message", b.cfg.MaxConcurrency)
		return
	}
	go func() {
		defer func() { <-b.sem }()
		defer quietRecover("answering a message")
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
		debugf("ignored duplicate from %s", req.Name)
		return
	}
	b.remember(req)
	if strings.EqualFold(strings.TrimSpace(req.Text), "/example") {
		b.example(req)
		return
	}
	infof("%s", req.logLine())
	b.respond(req)
}

// example answers "/example" without the model: what the bot can do, with a real
// @mention of whoever asked.
func (b *Bot) example(req *Request) {
	b.ensureSelfName()
	text := exampleText(req, b.introName(), modelName(b.cfg.Model), b.toolsText())
	if err := b.sig.Send(req.Target, text, buildMentions(text, b.knownNames(req.Key)), nil); err != nil {
		errorf("send to %s failed: %v", req.Name, err)
		return
	}
	infof("example for %s", req.Name)
}

func exampleText(req *Request, self, model, tools string) string {
	hi := "Hi!"
	if req.Target.GroupID != "" && req.Display != "" {
		hi = "Hi @" + req.Display + "!"
	}
	if tools == "none" {
		tools = "no tools"
	}
	return hi + " I'm " + self + ". Here's what I can do:\n" +
		"- Answer when you @mention me, reply to one of my messages, or DM me (if you're on my list)\n" +
		"- Use a message you quote as context\n" +
		"- Remember our conversation for up to a day (less with disappearing messages)\n" +
		"- Mention people by name in my replies\n" +
		"- Run " + model + " with " + tools + "\n" +
		"This reply didn't use the model."
}

// imageNote marks images in a prompt, e.g. "[1 image attached]".
func imageNote(n int, what string) string {
	if n == 1 {
		return "[1 image " + what + "]"
	}
	return fmt.Sprintf("[%d images %s]", n, what)
}

// images fetches the request's images as data: URLs for the model, if it can
// see images. One that cannot be fetched is skipped.
func (b *Bot) images(req *Request) []string {
	if !b.vision || len(req.Images) == 0 {
		return nil
	}
	var urls []string
	for _, a := range req.Images {
		ctx, cancel := context.WithTimeout(b.ctx, 15*time.Second)
		data, err := b.sig.GetAttachment(ctx, req.Target, a.ID)
		cancel()
		if err != nil || !sendableImage(data) {
			warnf("could not fetch an image from %s: %v", req.Name, cmp.Or(err, errors.New("not an image")))
			continue
		}
		urls = append(urls, "data:"+http.DetectContentType(data)+";base64,"+base64.StdEncoding.EncodeToString(data))
	}
	return urls
}

// sticker fetches the request's sticker as a data: URL for the model, if it
// can see images, else "".
func (b *Bot) sticker(req *Request) string {
	if !b.vision {
		return ""
	}
	ctx, cancel := context.WithTimeout(b.ctx, 15*time.Second)
	defer cancel()
	data, err := b.sig.GetSticker(ctx, *req.Sticker)
	if err != nil || !sendableImage(data) {
		warnf("could not fetch a sticker from %s: %v", req.Name, cmp.Or(err, errors.New("not an image")))
		return ""
	}
	return "data:" + http.DetectContentType(data) + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// duplicate reports whether the same sender sent the same text to the same
// chat within the dedupe window, and records this one.
func (b *Bot) duplicate(req *Request) bool {
	window := dedupeWindow
	key := req.Sender + "\x00" + req.Key + "\x00" + req.Text
	for _, a := range req.Images { // two different photos are not duplicates
		key += "\x00" + a.ID
	}
	if s := req.Sticker; s != nil {
		key += fmt.Sprintf("\x00%s:%d", s.PackID, s.StickerID)
	}
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

// forgetKnown drops the names remembered for a chat, because its members
// changed. People still in it are remembered again by their next message.
func (b *Bot) forgetKnown(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.known, key)
}

// refreshSoon reloads the contact list as soon as the rate limit allows. Calls
// made while a reload is already scheduled fold into it, so a burst of group
// changes costs one reload and at most one timer.
func (b *Bot) refreshSoon() {
	if b.closing.Load() || !b.refreshPending.CompareAndSwap(false, true) {
		return
	}
	time.AfterFunc(b.names.untilRefreshAllowed(), func() {
		b.refreshPending.Store(false)
		if !b.closing.Load() {
			b.refreshContacts()
		}
	})
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
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

func (b *Bot) respond(req *Request) {
	stopTyping := b.keepTyping(req.Target)
	defer stopTyping() // also if something below panics
	b.ensureSelfName()
	// The history keeps only a marker for images, never the pictures.
	prompt, seen := strings.TrimSpace(req.Prompt), b.images(req)
	switch {
	case len(seen) > 0:
		prompt += "\n" + imageNote(len(seen), "attached")
	case len(req.Images) > 0:
		prompt += "\n" + imageNote(len(req.Images), "attached, which you can't see with this model")
	}
	if req.Sticker != nil {
		ctx, cancel := context.WithTimeout(b.ctx, 10*time.Second)
		label := b.sig.StickerLabel(ctx, *req.Sticker)
		cancel()
		if url := b.sticker(req); url != "" {
			prompt += "\n[sticker attached" + label + "]"
			seen = append(seen, url)
		} else {
			prompt += "\n[sticker attached" + label + ", which you can't see]"
		}
	}
	prompt = strings.TrimSpace(prompt)
	req.Prompt = prompt
	msgs := append(b.getHistory(req.Key), ChatMsg{Role: "user", Content: prompt, Images: seen})

	start := time.Now()
	ctx, chart := b.ctx, b.toolCalls && wantsChart(req.Text)
	if chart {
		ctx = withChart(ctx)
	}
	res, err := b.ai.Complete(ctx, b.systemPrompt(chart), msgs)
	stopTyping()
	took := time.Since(start)
	if err != nil {
		errorf("OpenRouter error for %s after %.1fs: %v", req.Name, took.Seconds(), err)
		res = Completion{Text: "Sorry, I couldn't get an answer right now."}
	} else {
		infof("%s", replyLogLine(req.Name, took, res))
		gone := historyExpiry(start, req.Expires)
		b.addHistory(req.Key, ChatMsg{Role: "user", Content: req.Prompt, Expires: gone}, ChatMsg{Role: "assistant", Content: res.Text, Expires: gone})
	}

	if n := len([]rune(res.Text)); n > maxReplyRunes {
		warnf("reply to %s truncated from %d chars", req.Name, n)
	}
	parts := replyParts(res.Text, b.introName())
	var attachments []string
	for i, img := range res.Images {
		attachments = append(attachments, imageDataURI(img, fmt.Sprintf("image%d", i+1)))
	}
	if len(parts) == 0 {
		if len(attachments) == 0 {
			warnf("empty reply to %s, nothing sent", req.Name)
			return
		}
		parts = []string{""} // the images alone
	}
	known := b.knownNames(req.Key)
	for i, part := range parts {
		var att []string
		if i == 0 {
			att = attachments // with the first message
		}
		if err := b.sig.Send(req.Target, part, buildMentions(part, known), att); err != nil {
			errorf("send to %s failed: %v", req.Name, err)
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

// defaultIntro is used when INTRO is unset. {name}, {model} and {tools} are substituted.
const defaultIntro = "hi, I'm {name}, your open source assistant running {model} (tools: {tools}). " +
	"I respond to @mentions and replies to my messages. See " + readmeURL + " for details."

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
		warnf("own profile name not found; calling itself signalbot (INTRO can name it)")
	}
}

// introName is the name the model is told it has.
func (b *Bot) introName() string { return cmp.Or(b.selfName(), "signalbot") }

// modelName drops OpenRouter's provider prefix: "deepseek/deepseek-v4" is
// introduced as "deepseek-v4".
func modelName(model string) string {
	return model[strings.LastIndex(model, "/")+1:]
}

// systemPrompt is the built-in prompt, plus SYSTEM_PROMPT, plus the intro.
// toolsText lists the tools the model can use, for {tools} and /example.
func (b *Bot) toolsText() string {
	if !b.toolCalls {
		return "none"
	}
	tools := []string{"charts"}
	if b.cfg.Search {
		tools = []string{"web search", "charts"}
	}
	return strings.Join(append(tools, b.toolNames...), ", ")
}

// wantsChart reports whether a message asks for a chart, so the chart tool
// is offered only then.
var wantsChart = regexp.MustCompile(`(?i)\b(chart(s|ed|ing)?|graph(s|ed|ing)?|plot(s|ted|ting)?|visuali[sz](e|es|ed|ing|ation|ations))\b`).MatchString

// systemPrompt is the prompt for one request; chart says whether the chart
// tool is offered with it.
func (b *Bot) systemPrompt(chart bool) string {
	intro := strings.NewReplacer("{name}", safeName(b.introName()), "{model}", modelName(b.cfg.Model), "{tools}", b.toolsText()).Replace(b.cfg.Intro)
	var tools []string
	ask := "name them"
	if b.toolCalls {
		if b.cfg.Search {
			tools = append(tools, "web search")
		}
		if chart {
			tools = append(tools, "charts ("+chartTool+")")
		}
		if b.toolGroups != "" {
			tools, ask = append(tools, "MCP tools by server, "+b.toolGroups), "name them, grouped by server"
		}
	}
	p := basePrompt
	if len(tools) > 0 {
		p += "\nYour tools: " + strings.Join(tools, "; ") + ". If asked what tools you have, " + ask + "."
	}
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
	n = strings.ToLower(n) // mentions match case-insensitively, so "alice" claims "Alice"
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
	return unexpired(b.history[key], time.Now())
}

// maxHistoryAge is the longest any message is kept. A chat's shorter
// disappearing-messages timer wins.
const maxHistoryAge = 24 * time.Hour

// historyExpiry is when a message handled at now must be forgotten.
func historyExpiry(now time.Time, disappearing time.Duration) time.Time {
	if disappearing <= 0 {
		disappearing = maxHistoryAge
	}
	return now.Add(min(disappearing, maxHistoryAge))
}

// unexpired returns a copy of h without the messages whose disappearing-
// messages time has passed.
func unexpired(h []ChatMsg, now time.Time) []ChatMsg {
	return slices.DeleteFunc(slices.Clone(h), func(m ChatMsg) bool {
		return !m.Expires.IsZero() && !now.Before(m.Expires)
	})
}

// forget erases the chat's expired messages from memory.
func (b *Bot) forget(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if h := unexpired(b.history[key], time.Now()); len(h) > 0 {
		b.history[key] = h
	} else {
		delete(b.history, key)
	}
}

func (b *Bot) addHistory(key string, msgs ...ChatMsg) {
	b.mu.Lock()
	defer b.mu.Unlock()
	makeRoom(b.history, key, maxChats)
	h := append(unexpired(b.history[key], time.Now()), msgs...)
	if max := b.cfg.HistoryTurns * 2; len(h) > max {
		h = append([]ChatMsg(nil), h[len(h)-max:]...)
	}
	b.history[key] = h
	// Erase the expired messages and let the garbage collector reclaim them.
	if e := msgs[0].Expires; !e.IsZero() {
		time.AfterFunc(time.Until(e), func() { b.forget(key) })
	}
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

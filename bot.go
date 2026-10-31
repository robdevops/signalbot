package main

import (
	"cmp"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/kit/bound"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robdevops/signalbot/internal/kit/logx"

	"github.com/robdevops/signalbot/internal/kit/txt"
)

const (
	// maxReplyRunes caps a whole reply, so one request cannot fan out into
	// many messages.
	maxReplyRunes = 8000
	// maxChatterRunes caps one unaddressed message kept as context.
	maxChatterRunes = 2000
	// maxQuoteRunes caps quoted text spliced into a prompt.
	maxQuoteRunes = 500
	// maxChats bounds the chats remembered: chat keys come from the network.
	maxChats = 500
	// dedupeWindow: the same text from the same sender in the same chat
	// within this long is ignored.
	dedupeWindow = 10 * time.Second
)

type Bot struct {
	cfg        *Config
	transports map[string]chat.Transport // by kind: the ChatID prefix
	ai         Completer
	sem        chan struct{}
	ctx        context.Context
	closing    atomic.Bool // set by Shutdown: no new reply starts

	mu      sync.Mutex
	history map[string][]ChatMsg
	erasers map[string]eraser // one timer per chat, for its next message to expire
	charts  map[string]int    // chat key -> replies left with the chart tools on
	dedupe  map[string]time.Time

	typingEvery time.Duration
	toolNames   []string              // MCP tools the model may call, for {tools}
	toolGroups  string                // the same, grouped by server, for the system prompt
	commands    map[string]mcpCommand // chat commands, by name without the slash
	vision      bool                  // the model accepts images
	toolCalls   bool                  // the model can call tools (web search, charts, MCP)
}

func NewBot(ctx context.Context, cfg *Config, ai Completer, transports ...chat.Transport) *Bot {
	b := &Bot{
		cfg: cfg, ai: ai, ctx: ctx,
		transports:  map[string]chat.Transport{},
		sem:         make(chan struct{}, cfg.MaxConcurrency),
		history:     map[string][]ChatMsg{},
		erasers:     map[string]eraser{},
		charts:      map[string]int{},
		dedupe:      map[string]time.Time{},
		typingEvery: 10 * time.Second,
		toolCalls:   true,
	}
	for _, t := range transports {
		b.transports[t.Kind()] = t
	}
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

// Request is a message the bot has decided to answer.
type Request struct {
	Chat   chat.ChatID
	Group  bool
	Key    string // history key: the chat id
	Prompt string

	Kind    string // "mention", "reply" or "dm"
	Sender  string // display name and id, for the log
	Name    string // display name, else the id: short form for the log
	Display string // display name, "" if unknown
	Text    string // message text with the bot mention stripped

	Expires time.Duration // the chat's disappearing-messages timer, 0 if off
	Tool    string        // a chat command's tool, which the model must call
	Images  []chat.Media  // images in or quoted by the message, fetched when answering
	Sticker *chat.Media   // a sticker sent as the message, fetched when answering
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
	req := &Request{Chat: m.Chat, Group: m.Group, Key: string(m.Chat), Prompt: text, Kind: m.Why, Sender: who,
		Name: cmp.Or(name, m.Sender.ID), Display: name, Text: m.Text, Expires: m.Expires}
	for _, md := range m.Media {
		switch {
		case md.Kind == chat.Sticker:
			req.Sticker = &md
		case len(req.Images) < maxReplyImages:
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
func replyLogLine(name string, took time.Duration, c Completion) string {
	cached := 100 * min(c.CachedTokens, c.InputTokens) / max(c.InputTokens, 1)
	return fmt.Sprintf("reply %s %.1fs, tokens in/out: %d/%d (%d%% cached), $%.3f",
		name, took.Seconds(), c.InputTokens, c.OutputTokens, cached, c.Cost)
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
		// Not for the bot, and never logged. A group message is remembered as
		// context with GROUP_CONTEXT. Its Load is cheap: no network.
		if in.Group && b.cfg.GroupContext {
			if m, ok := in.Load(b.ctx); ok {
				b.rememberChatter(m)
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
	if strings.EqualFold(strings.TrimSpace(req.Text), "/example") {
		b.example(req)
		return
	}
	if name, rest, ok := parseCommand(req.Text); ok {
		if c, ok := b.commands[name]; ok {
			if c.RequireArg && rest == "" {
				b.sendText(req, "/"+name+" needs something after it.")
				return
			}
			req.Tool = c.Tool
			logx.Infof("chat command /%s from %s", name, req.Name)
		}
	}
	logx.Infof("%s", req.logLine())
	b.respond(req)
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

// sendText sends the bot's own text, not the model's, to req's chat.
func (b *Bot) sendText(req *Request, text string) {
	if err := b.tr(req.Chat).Send(b.ctx, chat.Outgoing{Chat: req.Chat, Text: text}); err != nil {
		logx.Errorf("send to %s failed: %v", req.Name, err)
	}
}

// example answers "/example" without the model: what the bot can do, with a real
// @mention of whoever asked.
func (b *Bot) example(req *Request) {
	text := exampleText(req, b.introName(req.Chat), modelName(b.cfg.Model), b.toolsText())
	if err := b.tr(req.Chat).Send(b.ctx, chat.Outgoing{Chat: req.Chat, Text: text}); err != nil {
		logx.Errorf("send to %s failed: %v", req.Name, err)
		return
	}
	logx.Infof("example for %s", req.Name)
}

func exampleText(req *Request, self, model, tools string) string {
	hi := "Hi!"
	if req.Group && req.Display != "" {
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

// rememberChatter adds a group message that isn't for the bot to the group's
// history, so a later "what do you think?" has the conversation to go on. It
// is kept like any other message (HISTORY_TURNS, a day at most, sooner with
// disappearing messages) and only text is kept: images are noted, not fetched.
func (b *Bot) rememberChatter(m chat.Message) {
	text := txt.Clamp(m.Text, maxChatterRunes)
	if slices.ContainsFunc(m.Media, func(md chat.Media) bool { return md.Kind == chat.Image }) {
		text = strings.TrimSpace(text + " [image]")
	}
	if slices.ContainsFunc(m.Media, func(md chat.Media) bool { return md.Kind == chat.Sticker }) {
		text = strings.TrimSpace(text + " [sticker]")
	}
	if text == "" {
		return
	}
	b.addHistory(string(m.Chat), ChatMsg{
		Role:    "user",
		Content: cmp.Or(m.Sender.Name, "someone") + ": " + text,
		Expires: historyExpiry(time.Now(), m.Expires),
	})
}

// notKept ends the history's marker for an image the model saw.
const notKept = "; you see images only in the latest message, so to look again they must reply to it and mention you"

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
		data, err := b.tr(req.Chat).Fetch(ctx, req.Chat, a)
		cancel()
		if err != nil || !sendableImage(data) {
			logx.Warnf("could not fetch an image from %s: %v", req.Name, cmp.Or(err, errors.New("not an image")))
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
	data, err := b.tr(req.Chat).Fetch(ctx, req.Chat, *req.Sticker)
	if err != nil || !sendableImage(data) {
		logx.Warnf("could not fetch a sticker from %s: %v", req.Name, cmp.Or(err, errors.New("not an image")))
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
		key += "\x00" + s.ID
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

// keepTyping sends a typing indicator now and every typingEvery until stopped.
func (b *Bot) keepTyping(c chat.ChatID) (stop func()) {
	done := make(chan struct{})
	t := b.tr(c)
	t.Typing(c)
	go func() {
		tick := time.NewTicker(b.typingEvery)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				t.Typing(c)
			}
		}
	}()
	var once sync.Once
	return func() { once.Do(func() { close(done) }) }
}

func (b *Bot) respond(req *Request) {
	stopTyping := b.keepTyping(req.Chat)
	defer stopTyping() // also if something below panics
	self := b.introName(req.Chat)
	// The history keeps only a marker for images, never the pictures, saying
	// so, or the model concludes later that it can't see images at all.
	prompt, seen := strings.TrimSpace(req.Prompt), b.images(req)
	kept := prompt
	switch {
	case len(seen) > 0:
		prompt += "\n" + imageNote(len(seen), "attached")
		kept += "\n" + imageNote(len(seen), "attached"+notKept)
	case len(req.Images) > 0:
		prompt += "\n" + imageNote(len(req.Images), "attached, which you can't see with this model")
		kept = prompt
	}
	if req.Sticker != nil {
		label := req.Sticker.Label
		if url := b.sticker(req); url != "" {
			prompt += "\n[sticker attached" + label + "]"
			kept += "\n[sticker attached" + label + notKept + "]"
			seen = append(seen, url)
		} else {
			prompt += "\n[sticker attached" + label + ", which you can't see]"
			kept += "\n[sticker attached" + label + ", which you can't see]"
		}
	}
	prompt = strings.TrimSpace(prompt)
	req.Prompt = strings.TrimSpace(kept)
	msgs := append(b.getHistory(req.Key), ChatMsg{Role: "user", Content: prompt, Images: seen})

	start := time.Now()
	ctx, asked := b.ctx, wantsChart(req.Text)
	chart := req.Tool == "" && b.toolCalls && (asked || b.chartTurns(req.Key) > 0)
	switch {
	case req.Tool != "":
		ctx = withTool(ctx, req.Tool)
	case chart:
		ctx = withChart(ctx)
	}
	res, err := b.ai.Complete(ctx, b.systemPrompt(chart, req.Chat, self), msgs)
	stopTyping()
	took := time.Since(start)
	var caption string
	if err != nil {
		logx.Errorf("OpenRouter error for %s after %.1fs: %v", req.Name, took.Seconds(), err)
		res = Completion{Text: "Sorry, I couldn't get an answer right now."}
	} else {
		logx.Infof("%s", replyLogLine(req.Name, took, res))
		// A chart is sent as one message captioned with its title, in place
		// of the model's text.
		if caption = chartCaptions(res); caption != "" {
			res.Text = ""
		}
		if chart {
			b.chartReplied(req.Key, asked || res.Charted)
		}
		gone := historyExpiry(start, req.Expires)
		b.addHistory(req.Key, ChatMsg{Role: "user", Content: req.Prompt, Expires: gone}, ChatMsg{Role: "assistant", Content: sentImages(res) + res.Text, Expires: gone})
	}

	if n := len([]rune(res.Text)); n > maxReplyRunes {
		logx.Warnf("reply to %s truncated from %d chars", req.Name, n)
	}
	out := chat.Outgoing{Chat: req.Chat, Text: replyText(res.Text, self)}
	if caption != "" {
		out.Text = caption
	}
	for i, img := range res.Images {
		var c string
		if i < len(res.Captions) {
			c = res.Captions[i]
		}
		out.Pictures = append(out.Pictures, chat.Picture{Data: img, Caption: c})
	}
	if strings.TrimSpace(out.Text) == "" && len(out.Pictures) == 0 {
		logx.Warnf("empty reply to %s, nothing sent", req.Name)
		return
	}
	if err := b.tr(req.Chat).Send(b.ctx, out); err != nil {
		logx.Errorf("send to %s failed: %v", req.Name, err)
	}
}

const truncatedMarker = " […truncated]"

// replyText is a model reply as sent: the bot's own @name stripped, and the
// whole reply capped at maxReplyRunes. The transport splits it.
func replyText(reply, self string) string {
	reply = stripSelfTags(reply, self)
	if r := []rune(reply); len(r) > maxReplyRunes {
		reply = string(r[:maxReplyRunes-len([]rune(truncatedMarker))]) + truncatedMarker
	}
	return reply
}

// stripSelfTags removes the bot's own @name from a reply, without leaving a
// double space behind.
func stripSelfTags(reply, self string) string {
	rs := []rune(reply)
	spans := txt.AtNames(rs, map[string]string{txt.Safe(self): "self"})
	if len(spans) == 0 {
		return reply
	}
	var out []rune
	pos := 0
	for _, s := range spans {
		out = append(out, rs[pos:s.Start]...)
		pos = s.End
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
// basePrompt follows the transport's description of the chat.
const basePrompt = " To mention someone, write @ followed by their exact name. Never write your own name with an @."

// introName is the name the model is told it has.
func (b *Bot) introName(c chat.ChatID) string { return cmp.Or(b.tr(c).Name(b.ctx), "signalbot") }

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
	return strings.Join(append(append(tools, b.toolNames...), b.commandNames()...), ", ")
}

// commandNames lists the chat commands as "/name", sorted.
func (b *Bot) commandNames() []string {
	var out []string
	for _, n := range slices.Sorted(maps.Keys(b.commands)) {
		out = append(out, "/"+n)
	}
	return out
}

// chartCaptions is the caption for a reply's charts, one per line, or ""
// if it drew none.
func chartCaptions(res Completion) string {
	var caps []string
	for _, c := range res.Captions {
		if c != "" {
			caps = append(caps, c)
		}
	}
	return strings.Join(caps, "\n")
}

// sentImages notes in the history the charts and images a reply sent, which
// the model would otherwise not know it did, e.g. "[sent the chart "NVDA"]".
func sentImages(res Completion) string {
	var note string
	for i := range res.Images {
		if i < len(res.Captions) && res.Captions[i] != "" {
			note += "[sent the chart " + strconv.Quote(res.Captions[i]) + "]\n"
		} else {
			note += "[sent an image]\n"
		}
	}
	return note
}

// chartFollowUps is how many replies after one that asked for or drew a chart
// still offer the chart tools, for follow-ups like "show that for a month".
const chartFollowUps = 2

func (b *Bot) chartTurns(key string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.charts[key]
}

// chartReplied counts a reply that offered the chart tools: one that asked
// for or drew a chart restarts the count, any other uses one up.
func (b *Bot) chartReplied(key string, fresh bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case fresh:
		bound.MakeRoom(b.charts, key, maxChats)
		b.charts[key] = chartFollowUps
	case b.charts[key] > 1:
		b.charts[key]--
	default:
		delete(b.charts, key)
	}
}

// wantsChart reports whether a message asks for a chart, so the chart tool
// is offered only then.
var wantsChart = regexp.MustCompile(`(?i)\b(chart(s|ed|ing)?|graph(s|ed|ing)?|plot(s|ted|ting)?|visuali[sz](e|es|ed|ing|ation|ations)|pies?|donuts?|doughnuts?)\b`).MatchString

// systemPrompt is the prompt for one request; chart says whether the chart
// tool is offered with it.
func (b *Bot) systemPrompt(chart bool, c chat.ChatID, self string) string {
	intro := strings.NewReplacer("{name}", txt.Safe(self), "{model}", modelName(b.cfg.Model), "{tools}", b.toolsText()).Replace(b.cfg.Intro)
	var tools []string
	ask := "name them"
	if b.toolCalls {
		if b.cfg.Search {
			tools = append(tools, "web search")
		}
		if chart {
			tools = append(tools, "charts ("+chartTool+", "+donutTool+")")
		}
		if b.toolGroups != "" {
			tools, ask = append(tools, "MCP tools by server, "+b.toolGroups), "name them, grouped by server"
		}
		if cmds := b.commandNames(); len(cmds) > 0 {
			tools = append(tools, "chat commands "+strings.Join(cmds, ", "))
		}
	}
	p := b.tr(c).Describe() + basePrompt
	if len(tools) > 0 {
		p += "\nYour tools: " + strings.Join(tools, "; ") + ". If asked what tools you have, " + ask + "."
	}
	if b.cfg.ExtraPrompt != "" {
		p += "\n" + b.cfg.ExtraPrompt
	}
	// Quote so a newline or quote in the name or in INTRO cannot escape it.
	return p + "\nIf asked to introduce yourself, reply with exactly this text and nothing else: " + strconv.Quote(intro)
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

// eraser is a chat's pending timer and when it fires.
type eraser struct {
	timer *time.Timer
	at    time.Time
}

// forget erases the chat's expired messages from memory, and sets the timer
// for the next one.
func (b *Bot) forget(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.erasers, key)
	if h := unexpired(b.history[key], time.Now()); len(h) > 0 {
		b.history[key] = h
	} else {
		delete(b.history, key)
		delete(b.charts, key) // nothing left to follow up on
		if t := b.tr(chat.ChatID(key)); t != nil {
			t.Forget(chat.ChatID(key)) // nor names to mention
		}
	}
	b.scheduleErase(key)
}

// scheduleErase makes sure a timer will fire when the chat's earliest message
// expires. One timer per chat, however many messages, so a busy group can't
// pile up timers. Caller holds b.mu.
func (b *Bot) scheduleErase(key string) {
	var next time.Time
	for _, m := range b.history[key] {
		if !m.Expires.IsZero() && (next.IsZero() || m.Expires.Before(next)) {
			next = m.Expires
		}
	}
	e, ok := b.erasers[key]
	switch {
	case next.IsZero():
		if ok {
			e.timer.Stop()
			delete(b.erasers, key)
		}
	case !ok || next.Before(e.at):
		if ok {
			e.timer.Stop()
		}
		b.erasers[key] = eraser{time.AfterFunc(time.Until(next), func() { b.forget(key) }), next}
	}
}

func (b *Bot) addHistory(key string, msgs ...ChatMsg) {
	b.mu.Lock()
	defer b.mu.Unlock()
	bound.MakeRoom(b.history, key, maxChats)
	h := append(unexpired(b.history[key], time.Now()), msgs...)
	if max := b.cfg.HistoryTurns * 2; len(h) > max {
		h = append([]ChatMsg(nil), h[len(h)-max:]...)
	}
	b.history[key] = h
	// Erase the expired messages and let the garbage collector reclaim them.
	b.scheduleErase(key)
}

package bot

import (
	"cmp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/history"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
	"github.com/robdevops/signalbot/internal/llm"
)

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
	p := b.prepare(req, self)
	t := p.turn
	msgs := append(b.getHistory(req.Key), llm.Message{Role: "user", Content: p.prompt, Images: p.images})

	start := time.Now()
	res, err := b.ai.Complete(b.ctx, llm.Request{System: p.system, Messages: msgs, Tools: p.tools, Force: req.Tool})
	stopTyping()
	took := time.Since(start)
	var replaced *chat.Outgoing
	if err != nil {
		logx.Errorf("model error for %s after %.1fs: %v", req.Name, took.Seconds(), err)
		res = llm.Completion{Text: "Sorry, I couldn't get an answer right now."}
	} else {
		logx.Infof("%s", replyLogLine(req.Name, took, res))
		reply := feature.Reply{Text: res.Text, Pictures: res.Images, Used: res.Used}
		for _, fn := range b.hooks.reply {
			if out := fn(t, reply); out != nil {
				replaced = out
			}
		}
		if replaced != nil {
			res.Text = "" // sent in its place, never seen
		}
		gone := history.Expiry(start, req.Expires)
		b.addHistory(req.Key, history.Entry{Role: "user", Content: req.Prompt, Expires: gone}, history.Entry{Role: "assistant", Content: sentImages(res) + res.Text, Expires: gone})
	}

	if n := len([]rune(res.Text)); n > maxReplyRunes {
		logx.Warnf("reply to %s truncated from %d chars", req.Name, n)
	}
	out := chat.Outgoing{Chat: req.Chat, Text: replyText(res.Text, self), Pictures: res.Images}
	if replaced != nil {
		out.Text = replaced.Text
	}
	if strings.TrimSpace(out.Text) == "" && len(out.Pictures) == 0 {
		logx.Warnf("empty reply to %s, nothing sent", req.Name)
		return
	}
	if err := b.tr(req.Chat).Send(b.ctx, out); err != nil {
		logx.Errorf("send to %s failed: %v", req.Name, err)
	}
}

// prepared is a turn ready for the model.
type prepared struct {
	turn   feature.Turn
	system string
	prompt string   // the message as the model sees it
	images []string // shown with it
	tools  []feature.Tool
}

// prepare offers the turn's tools and runs the OnTurn hooks: prompt lines,
// notes on the message (history keeps its own version of each, in
// req.Prompt), and images for the model.
func (b *Bot) prepare(req *Request, self string) prepared {
	t := feature.Turn{Message: req.Message, Bot: self, Forced: req.Tool}
	groups, tools := b.offer(t)
	for _, tl := range tools {
		t.Offered = append(t.Offered, tl.Name)
	}
	prompt, kept := strings.TrimSpace(req.Prompt), strings.TrimSpace(req.Prompt)
	modelNote, historyNote, images := b.see(b.ctx, req.Message)
	if modelNote != "" {
		prompt += "\n" + modelNote
		kept += "\n" + historyNote
	}
	req.Prompt = strings.TrimSpace(kept)
	return prepared{turn: t, system: b.systemPrompt(req.Chat, groups, self), prompt: strings.TrimSpace(prompt), images: images, tools: tools}
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

// sentImages notes in the history the charts and images a reply sent, which
// the model would otherwise not know it did, e.g. "[sent the chart "NVDA"]".
func sentImages(res llm.Completion) string {
	var note string
	for _, img := range res.Images {
		if img.Caption != "" {
			note += "[sent the chart " + strconv.Quote(img.Caption) + "]\n"
		} else {
			note += "[sent an image]\n"
		}
	}
	return note
}

// getHistory is the chat's history as model messages.
func (b *Bot) getHistory(key string) []llm.Message {
	var out []llm.Message
	for _, e := range b.hist.Get(chat.ChatID(key)) {
		out = append(out, llm.Message{Role: e.Role, Content: e.Content})
	}
	return out
}

// addHistory remembers entries in the chat's history.
func (b *Bot) addHistory(key string, entries ...history.Entry) {
	b.hist.Add(chat.ChatID(key), entries...)
}

// DefaultIntro is how the bot introduces itself when INTRO is unset. {name},
// {model} and {tools} are filled in.
const DefaultIntro = "hi, I'm {name}, your open source assistant running {model} (tools: {tools}). " +
	"I respond to @mentions and replies to my messages. See https://github.com/robdevops/signalbot/blob/main/README.md for details."

// basePrompt follows the transport's description of the chat.
const basePrompt = " To mention someone, write @ followed by their exact name. Never write your own name with an @."

// introName is the name the model is told it has.
func (b *Bot) introName(c chat.ChatID) string { return cmp.Or(b.tr(c).Name(b.ctx), "signalbot") }

// modelName drops OpenRouter's provider prefix: "deepseek/deepseek-v4" is
// introduced as "deepseek-v4".
func modelName(model string) string {
	return model[strings.LastIndex(model, "/")+1:]
}

// toolsText lists the tool groups by name, for {tools} and /example.
func (b *Bot) toolsText() string {
	if !b.cfg.Caps.Tools {
		return "none"
	}
	var names []string
	for _, g := range b.hooks.groups {
		if g.Intro != "" {
			names = append(names, g.Intro)
		}
	}
	return strings.Join(names, ", ")
}

// systemPrompt is the prompt for one request: the transport's description
// of the chat and the built-in rules, the tool groups offered, SYSTEM_PROMPT,
// and how to introduce itself.
func (b *Bot) systemPrompt(c chat.ChatID, groups []feature.ToolGroup, self string) string {
	p := b.tr(c).Describe() + basePrompt
	var names []string
	ask := "name them"
	for _, g := range groups {
		if g.Prompt != "" {
			names = append(names, g.Prompt)
		}
		if g.Grouped {
			ask = "name them, grouped by server"
		}
	}
	if len(names) > 0 {
		p += "\nYour tools: " + strings.Join(names, "; ") + ". If asked what tools you have, " + ask + "."
	}
	if b.cfg.ExtraPrompt != "" {
		p += "\n" + b.cfg.ExtraPrompt
	}
	intro := strings.NewReplacer("{name}", txt.Safe(self), "{model}", modelName(b.cfg.Model), "{tools}", b.toolsText()).
		Replace(cmp.Or(b.cfg.Intro, DefaultIntro))
	// Quoted so a newline or quote in the name or in INTRO cannot escape it.
	return p + "\nIf asked to introduce yourself, reply with exactly this text and nothing else: " + strconv.Quote(intro)
}

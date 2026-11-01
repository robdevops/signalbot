package bot

import (
	"cmp"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/robdevops/signalbot/internal/chats"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
	"github.com/robdevops/signalbot/internal/model"
)

const (
	// basePrompt and mentionPrompt always apply, with the network's own
	// line between them; MODEL_SYSTEM_PROMPT is added to them.
	basePrompt = "You are a helpful assistant in a chat. Reply in Markdown. " +
		"Aim for a single line. Two or three sentences at the very most, and only when asked for detail."
	mentionPrompt = "A mention shown as [unnamed user] is someone whose name you don't know; don't invent one. " +
		"To mention someone, write @ followed by their exact name. Never write your own name with an @."
	// maxQuote caps quoted text in a prompt.
	maxQuote = 500
	// followUps is how many replies after one that matched a tool group's
	// keywords, or used its tools, still offer it.
	followUps = 2
	sorry     = "Sorry, I couldn't get an answer right now."
)

// respond acts on an addressed message: as act says, else by asking the
// model and sending its reply (asking again if the API rejects its images
// or tools; see fallBack); with no model, only an Answer is sent. text
// is the message as the model sees it, prompt(msg) for one message; tools
// are offered by msg.Text alone, what was typed, never a sender's name or a
// quote.
func (b *Bot) respond(msg feature.Message, act feature.Action, text string) {
	switch {
	case act.Answer != "":
		b.net.Send(msg.Chat, act.Answer, nil, b.chats.Names(msg.Chat.Key()))
		return
	case b.ai == nil:
		return // no model: no typing, no reply
	case msg.Text == "" && len(b.turns) == 0:
		return // nothing to answer
	}
	stop := b.net.Typing(msg.Chat)
	defer stop() // also if something below panics
	key := msg.Chat.Key()
	start := time.Now() // history expires from here, not from when it is stored
	var reply model.Reply
	var kept string
	var offered []int
	var err error
	for {
		asked, added := text, false
		var images []string
		kept = text
		for _, fn := range b.turns {
			a := fn(b.ctx, msg)
			if a.Note != "" {
				asked, kept, added = asked+"\n"+a.Note, kept+"\n"+cmp.Or(a.HistoryNote, a.Note), true
			}
			images = append(images, a.Images...)
		}
		if !b.images.Load() {
			images = nil
		}
		if msg.Text == "" && !added && len(images) == 0 {
			return
		}
		var tools []feature.Tool
		offered, tools = b.offer(key, msg.Text, act.ToolChoice)
		reply, err = b.ai.Ask(b.ctx, model.Request{
			System:     b.system(offered),
			History:    b.chats.History(key),
			Text:       strings.TrimSpace(asked),
			Images:     images,
			Tools:      tools,
			ToolChoice: act.ToolChoice,
		})
		if !b.fallBack(err) {
			break
		}
	}
	stop()
	if err != nil {
		logx.Errorf("model error after %.1fs: %v", time.Since(start).Seconds(), err)
		b.net.Send(msg.Chat, sorry, nil, b.chats.Names(key))
		return
	}
	b.followUp(key, offered, msg.Text, reply.ToolsUsed)
	// History notes the pictures sent, which the model would otherwise not
	// know it did. Pictures with captions are sent with those in place of
	// the text, which neither goes out nor is kept.
	var notes string
	var captions []string
	var pics [][]byte
	for _, p := range reply.Pictures {
		notes += cmp.Or(p.HistoryNote, "[sent an image]") + "\n"
		if p.Caption != "" {
			captions = append(captions, p.Caption)
		}
		pics = append(pics, p.Data)
	}
	sent := reply.Text
	if len(captions) > 0 {
		reply.Text, sent = "", strings.Join(captions, "\n")
	}
	b.chats.Add(key, start, msg.Expires, chats.Entry{Role: "user", Content: strings.TrimSpace(kept)},
		chats.Entry{Role: "assistant", Content: notes + reply.Text})
	b.net.Send(msg.Chat, sent, pics, b.chats.Names(key))
}

// fallBack reports whether err is the API rejecting a request for its
// images or tools, which it turns off for the rest of the run, with one
// warning, so the turn is asked again without them. That ends: with
// images off none are sent, and with tools off none are offered.
func (b *Bot) fallBack(err error) bool {
	on, what := &b.images, "images"
	switch {
	case errors.Is(err, model.ErrTools):
		on, what = &b.callsTools, "tools"
	case !errors.Is(err, model.ErrImages):
		return false
	}
	if on.CompareAndSwap(true, false) {
		logx.Warnf("the model's API rejected a request with %s, so %s are off: %v", what, what, err)
	}
	return true
}

// prompt is the message as the model sees it: its quote, escaped and capped
// so it can't pose as a turn of its own, and in a group the sender's name.
func prompt(msg feature.Message) string {
	text := msg.Text
	if q := msg.Quote; q != nil && q.Text != "" {
		quoted := strconv.Quote(txt.Clamp(q.Text, maxQuote))
		if q.Bot {
			text = "[Replying to a message attributed to you: " + quoted + "]\n" + text
		} else {
			text = "[Replying to a message by " + strconv.Quote(cmp.Or(q.Author, "someone")) + ": " + quoted + "]\n" + text
		}
	}
	if msg.Chat.GroupID != "" && msg.Name != "" {
		text = msg.Name + ": " + text
	}
	return strings.TrimSpace(text)
}

// offer picks the tool groups offered with a message, by index, and their
// tools. A group without Keywords is always offered; one with them when the
// text matches, or while it has follow-ups left. A tool choice (a chat
// command) adds its tool to whatever else is offered.
func (b *Bot) offer(key, text, choice string) (offered []int, tools []feature.Tool) {
	if !b.callsTools.Load() {
		return nil, nil
	}
	for i, g := range b.groups {
		chosen := slices.IndexFunc(g.Tools, func(t feature.Tool) bool { return choice != "" && t.Name == choice })
		switch {
		case g.Keywords == nil || g.Keywords.MatchString(text) || b.chats.FollowUp(key, i) > 0:
			offered, tools = append(offered, i), append(tools, g.Tools...)
		case chosen >= 0:
			offered, tools = append(offered, i), append(tools, g.Tools[chosen])
		}
	}
	return offered, tools
}

// followUp counts down the follow-ups of each offered group with Keywords,
// and starts them again for one the text matched or whose tools were used.
func (b *Bot) followUp(key string, offered []int, text string, used []string) {
	for _, i := range offered {
		g := b.groups[i]
		if g.Keywords == nil {
			continue
		}
		n := b.chats.FollowUp(key, i) - 1
		if g.Keywords.MatchString(text) || slices.ContainsFunc(g.Tools, func(t feature.Tool) bool { return slices.Contains(used, t.Name) }) {
			n = followUps
		}
		b.chats.SetFollowUp(key, i, n)
	}
}

// toolNames is every tool group's Names in feature order, for {tools}; none
// if the model can't call tools.
func (b *Bot) toolNames() []string {
	if !b.callsTools.Load() {
		return nil
	}
	var out []string
	for _, g := range b.groups {
		out = append(out, g.Names...)
	}
	return out
}

// system is the system prompt: the base rules with the network's line, the
// tools line, the offered groups' lines, MODEL_SYSTEM_PROMPT and the intro.
func (b *Bot) system(offered []int) string {
	p := basePrompt
	if n := b.net.Prompt(); n != "" {
		p += " " + n
	}
	p += " " + mentionPrompt
	names := b.toolNames()
	if len(names) > 0 {
		var items []string
		for _, i := range offered {
			if l := b.groups[i].Listing; l != "" {
				items = append(items, l)
			}
		}
		p += "\n"
		if len(items) > 0 {
			p += "Your tools: " + strings.Join(items, "; ") + ". "
		}
		p += "If asked what tools you have: " + strings.Join(names, "; ") + "."
	}
	for _, i := range offered {
		if s := b.groups[i].System; s != "" {
			p += "\n" + s
		}
	}
	if b.cfg.SystemPrompt != "" {
		p += "\n" + b.cfg.SystemPrompt
	}
	// Quoted, so a newline or quote in the name or INTRO can't escape it.
	return p + "\nIf asked to introduce yourself, reply with exactly this text and nothing else: " + strconv.Quote(b.intro())
}

// intro is INTRO with {name}, {model} and {tools} filled in: what the model
// says when asked to introduce itself, which features may also send.
func (b *Bot) intro() string {
	model := b.cfg.Model[strings.LastIndex(b.cfg.Model, "/")+1:] // without the provider
	if b.ai == nil {
		model = "no model"
	}
	more := "\n\n" + strings.Join(b.more, "\n\n") + "\n\n" // each feature line padded with blank lines
	s := strings.NewReplacer("{name}", b.net.BotName(), "{model}", model, "{more}", more,
		"{tools}", cmp.Or(strings.Join(b.toolNames(), "\n\n"), "none")).Replace(b.cfg.Intro)
	s = lineEnds.ReplaceAllString(s, "\n")
	return strings.TrimSpace(blankLines.ReplaceAllString(s, "\n\n")) // never two blank lines in a row
}

var (
	lineEnds   = regexp.MustCompile(`[ \t]*\n[ \t]*`) // spaces either side of a line break
	blankLines = regexp.MustCompile(`\n{3,}`)         // two or more blank lines in a row
)

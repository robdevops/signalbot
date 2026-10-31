package bot

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/robdevops/signalbot/internal/chats"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/img"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
	"github.com/robdevops/signalbot/internal/openrouter"
	"github.com/robdevops/signalbot/internal/signal"
)

const (
	// basePrompt always applies; MODEL_SYSTEM_PROMPT is added to it.
	basePrompt = "You are a helpful assistant in a Signal chat. " +
		"Reply in plain text: Signal does not render markdown. " +
		"Aim for a single line. Two or three sentences at the very most, and only when asked for detail. " +
		"Never exceed 2000 characters, which is Signal's limit for one message. " +
		"A mention shown as [unnamed user] is someone whose name you don't know; don't invent one. " +
		"To mention someone, write @ followed by their exact name. Never write your own name with an @."
	// maxQuote caps quoted text in a prompt.
	maxQuote = 500
	// followUps is how many replies after one that matched a tool group's
	// keywords, or used its tools, still offer it.
	followUps = 2
	sorry     = "Sorry, I couldn't get an answer right now."
)

// respond acts on an addressed message: as act says, else by asking the
// model and sending its reply.
func (b *Bot) respond(msg feature.Message, act feature.Action) {
	switch {
	case act.Ignore:
		return
	case act.Answer != "":
		b.send(msg, act.Answer, nil, b.chats.Names(msg.Chat.Key()))
		return
	case msg.Text == "" && len(b.turns) == 0:
		return // nothing to answer
	}
	stop := b.sig.Typing(msg.Chat)
	defer stop() // also if something below panics
	key, turn := msg.Chat.Key(), feature.Turn{Message: msg}
	text := prompt(msg)
	kept, added := text, false
	var systems, images []string
	for _, fn := range b.turns {
		a := fn(b.ctx, turn)
		if a.System != "" {
			systems = append(systems, a.System)
		}
		if a.Note != "" {
			text, kept, added = text+"\n"+a.Note, kept+"\n"+cmp.Or(a.HistoryNote, a.Note), true
		}
		images = append(images, a.Images...)
	}
	if msg.Text == "" && !added && len(images) == 0 {
		return
	}
	offered, tools := b.offer(key, msg.Text, act.ToolChoice)
	start := time.Now() // history expires from here, not from when it is stored
	reply, err := b.ai.Ask(b.ctx, openrouter.Request{
		Who:        cmp.Or(msg.Name, msg.Sender),
		System:     b.system(offered, systems),
		History:    b.chats.History(key),
		Text:       strings.TrimSpace(text),
		Images:     images[:min(len(images), feature.MaxPictures)],
		Tools:      tools,
		ToolChoice: act.ToolChoice,
	})
	stop()
	if err != nil {
		logx.Errorf("OpenRouter error for %s after %.1fs: %v", cmp.Or(msg.Name, msg.Sender), time.Since(start).Seconds(), err)
		b.send(msg, sorry, nil, b.chats.Names(key))
		return
	}
	b.followUp(key, offered, msg.Text, reply.ToolsUsed)
	for _, fn := range b.replies {
		fn(turn, &reply)
	}
	// History notes the pictures sent, which the model would otherwise not
	// know it did. A reply with no text is sent with their captions.
	var notes string
	var captions, attachments []string
	for i, p := range reply.Pictures {
		notes += cmp.Or(p.HistoryNote, "[sent an image]") + "\n"
		if p.Caption != "" {
			captions = append(captions, p.Caption)
		}
		attachments = append(attachments, img.Attachment(p.Data, fmt.Sprintf("image%d", i+1)))
	}
	b.chats.Add(key, start, msg.Expires, chats.Entry{Role: "user", Content: strings.TrimSpace(kept)},
		chats.Entry{Role: "assistant", Content: notes + reply.Text})
	b.send(msg, cmp.Or(reply.Text, strings.Join(captions, "\n")), attachments, b.chats.Names(key))
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
// text matches, or while it has follow-ups left. A tool choice offers only
// that tool, and its group.
func (b *Bot) offer(key, text, choice string) (offered []int, tools []feature.Tool) {
	if !b.callsTools {
		return nil, nil
	}
	for i, g := range b.groups {
		chosen := slices.IndexFunc(g.Tools, func(t feature.Tool) bool { return choice != "" && t.Name == choice })
		wanted := choice == "" && g.Keywords != nil && (g.Keywords.MatchString(text) || b.chats.FollowUp(key, i) > 0)
		if g.Keywords != nil && !wanted && chosen < 0 {
			continue
		}
		offered = append(offered, i)
		switch {
		case choice == "":
			tools = append(tools, g.Tools...)
		case chosen >= 0:
			tools = append(tools, g.Tools[chosen])
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
	if !b.callsTools {
		return nil
	}
	var out []string
	for _, g := range b.groups {
		out = append(out, g.Names...)
	}
	return out
}

// system is the system prompt: the base rules, the tools line, the offered
// groups' and the hooks' lines, MODEL_SYSTEM_PROMPT and the intro.
func (b *Bot) system(offered []int, lines []string) string {
	p := basePrompt
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
		p += "If asked what tools you have: " + strings.Join(names, ", ") + "."
	}
	for _, i := range offered {
		if s := b.groups[i].System; s != "" {
			p += "\n" + s
		}
	}
	for _, l := range lines {
		p += "\n" + l
	}
	if b.cfg.SystemPrompt != "" {
		p += "\n" + b.cfg.SystemPrompt
	}
	model := b.cfg.Model[strings.LastIndex(b.cfg.Model, "/")+1:] // without the provider
	intro := strings.NewReplacer("{name}", b.sig.BotName(), "{model}", model,
		"{tools}", cmp.Or(strings.Join(names, ", "), "none")).Replace(b.cfg.Intro)
	// Quoted, so a newline or quote in the name or INTRO can't escape it.
	return p + "\nIf asked to introduce yourself, reply with exactly this text and nothing else: " + strconv.Quote(intro)
}

// send posts text to msg's chat, linking each @Name in known (lowercased
// name to ID), split into messages, with the attachments on the first.
func (b *Bot) send(msg feature.Message, text string, attachments []string, known map[string]string) {
	who := cmp.Or(msg.Name, msg.Sender)
	if n := utf8.RuneCountInString(text); n > signal.MaxReply {
		logx.Warnf("reply to %s truncated from %d chars", who, n)
	}
	parts := signal.Parts(text, b.sig.BotName())
	if len(parts) == 0 {
		if len(attachments) == 0 {
			logx.Warnf("empty reply to %s, nothing sent", who)
			return
		}
		parts = []string{""}
	}
	for i, part := range parts {
		if i > 0 {
			attachments = nil
		}
		if err := b.sig.Send(msg.Chat, part, signal.Mentions(part, known), attachments); err != nil {
			logx.Errorf("send to %s failed: %v", who, err)
			return
		}
	}
}

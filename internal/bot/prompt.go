package bot

import (
	"cmp"
	"strings"

	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/feature"
)

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
// and the lines features add.
func (b *Bot) systemPrompt(c chat.ChatID, groups []feature.ToolGroup, lines []string) string {
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
	for _, l := range lines {
		p += "\n" + l
	}
	return p
}

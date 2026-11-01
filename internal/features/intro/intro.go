// Package intro answers "/intro" without the model: what the bot can
// do, with a real @mention of whoever asked in a group.
package intro

import (
	"context"
	"strings"

	"github.com/robdevops/signalbot/internal/feature"
)

// Register adds /intro to h.
func Register(h feature.Host) error {
	return feature.Command(h, "intro", func(_ context.Context, m feature.Message, args string) feature.Action {
		if args != "" {
			return feature.Action{} // only "/intro" itself; anything more carries on
		}
		return feature.Action{Answer: text(m, h.Info())}
	})
}

func text(m feature.Message, info feature.Info) string {
	hi := "Hi!"
	if m.Chat.GroupID != "" && m.Name != "" {
		hi = "Hi @" + m.Name + "!"
	}
	tools := "no tools"
	if len(info.Tools) > 0 {
		tools = strings.Join(info.Tools, ", ")
	}
	// "deepseek/deepseek-v4" is introduced as "deepseek-v4".
	run := "Run " + info.Model[strings.LastIndex(info.Model, "/")+1:] + " with " + tools
	chat := "- Answer when you @mention me, reply to one of my messages, or DM me (if you're on my list)\n" +
		"- Use a message you quote as context\n" +
		"- Remember our conversation for up to a day (less with disappearing messages)\n"
	if info.Model == "" { // only commands are answered, and nothing is remembered
		run, chat = "Run no model, so I answer only commands", ""
	}
	return hi + " I'm " + info.Name + ". Here's what I can do:\n" + chat +
		"- Mention people by name in my replies\n" +
		"- " + run + "\n" +
		"This message did not use the model."
}

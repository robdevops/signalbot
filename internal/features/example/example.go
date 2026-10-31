// Package example answers "/example" without the model: what the bot can
// do, with a real @mention of whoever asked. It is also the template for
// adding your own commands.
package example

import (
	"cmp"
	"context"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

type Feature struct{}

func (Feature) Name() string { return "example" }

func (Feature) Register(h feature.Host) error {
	return h.Command("example", func(_ context.Context, t feature.Turn, rest string) (feature.Command, bool) {
		if rest != "" {
			return feature.Command{}, false // "/example foo" is for the model
		}
		m := t.Message
		logx.Infof("example for %s", cmp.Or(m.Sender.Name, m.Sender.ID))
		return feature.Command{Text: Text(m.Group, m.Sender.Name, t.Bot, h.Info())}, true
	})
}

// Text is the reply: a group mention of who asked, then what the bot does.
func Text(group bool, asker, self string, info feature.Info) string {
	hi := "Hi!"
	if group && asker != "" {
		hi = "Hi @" + asker + "!"
	}
	tools := info.Tools
	if tools == "none" {
		tools = "no tools"
	}
	return hi + " I'm " + self + ". Here's what I can do:\n" +
		"- Answer when you @mention me, reply to one of my messages, or DM me (if you're on my list)\n" +
		"- Use a message you quote as context\n" +
		"- Remember our conversation for up to a day (less with disappearing messages)\n" +
		"- Mention people by name in my replies\n" +
		"- Run " + info.Model + " with " + tools + "\n" +
		"This reply didn't use the model."
}

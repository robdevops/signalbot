// Package intro answers "/intro", and its alias "/help", without the model,
// with the bot's introduction (INTRO), the same text the model gives when
// asked: a DM's in a DM.
package intro

import (
	"context"

	"github.com/robdevops/signalbot/internal/feature"
)

// Register adds /intro and /help to h.
func Register(h feature.Host) error {
	answer := func(_ context.Context, m feature.Message, args string) feature.Action {
		if args != "" {
			return feature.Action{} // only the command itself; anything more carries on
		}
		info := h.Info()
		intro := info.Intro
		if m.Chat.GroupID == "" {
			intro = info.IntroDM
		}
		if info.Model != "" { // with no model, nothing does
			intro += "\n\nThis message did not use the model."
		}
		return feature.Action{Answer: intro}
	}
	for _, name := range []string{"intro", "help"} {
		if err := feature.Command(h, name, answer); err != nil {
			return err
		}
	}
	return nil
}

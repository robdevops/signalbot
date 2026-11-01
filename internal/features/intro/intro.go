// Package intro answers "/intro" without the model, with the bot's
// introduction (INTRO), the same text the model gives when asked.
package intro

import (
	"context"

	"github.com/robdevops/signalbot/internal/feature"
)

// Register adds /intro to h.
func Register(h feature.Host) error {
	return feature.Command(h, "intro", func(_ context.Context, _ feature.Message, args string) feature.Action {
		if args != "" {
			return feature.Action{} // only "/intro" itself; anything more carries on
		}
		return feature.Action{Answer: h.Info().Intro + "\n\nThis message did not use the model."}
	})
}

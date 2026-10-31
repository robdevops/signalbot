// Package groupctx keeps group messages that aren't for the bot as context,
// so a later "what do you think?" has the conversation to go on. They are
// kept like any other message (HISTORY_TURNS, a day at most, sooner with
// disappearing messages), and only as text: images are noted, not fetched.
// GROUP_CONTEXT turns it off.
package groupctx

import (
	"cmp"
	"slices"
	"strings"

	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// maxRunes caps one message kept as context.
const maxRunes = 2000

type Feature struct{}

func (Feature) Name() string { return "groupctx" }

func (Feature) Register(h feature.Host) error {
	if !h.Env().Bool("GROUP_CONTEXT", true) {
		return nil
	}
	h.OnUnaddressed(func(m chat.Message) {
		if text := Text(m); text != "" {
			h.Remember(m.Chat, cmp.Or(m.Sender.Name, "someone")+": "+text, m.Expires)
		}
	})
	return nil
}

// Text is what is kept of a message: its text, and notes for its images
// and sticker.
func Text(m chat.Message) string {
	text := txt.Clamp(m.Text, maxRunes)
	if slices.ContainsFunc(m.Media, func(md chat.Media) bool { return md.Kind == chat.Image }) {
		text = strings.TrimSpace(text + " [image]")
	}
	if slices.ContainsFunc(m.Media, func(md chat.Media) bool { return md.Kind == chat.Sticker }) {
		text = strings.TrimSpace(text + " [sticker]")
	}
	return text
}

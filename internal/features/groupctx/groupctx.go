// Package groupctx keeps group messages that aren't for the bot as context,
// so a later "what do you think?" has the conversation to go on.
package groupctx

import (
	"cmp"
	"context"
	"slices"
	"strings"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// maxChars caps the text kept: Signal's limit for one message.
const maxChars = 2000

// Register keeps group chatter on h unless GROUP_CONTEXT turns it off.
func Register(h feature.Host) error {
	if txt.Bool(h.Env("GROUP_CONTEXT", ""), true) {
		h.OnMessage(func(_ context.Context, m feature.Message) feature.Action {
			return feature.Action{Context: chatter(m)}
		})
	}
	return nil
}

// chatter is what history keeps of a message, as "<name>: <text>". The core
// keeps it only for an unaddressed group message, like any other message
// (HISTORY_TURNS, a day at most, sooner with disappearing messages), and
// never logs it. Only text is kept: images are noted, not fetched.
func chatter(m feature.Message) string {
	text := txt.Clamp(m.Text, maxChars)
	if slices.ContainsFunc(m.Media, func(md feature.Media) bool { return md.ID != "" && !md.Quoted }) {
		text = strings.TrimSpace(text + " [image]")
	}
	if slices.ContainsFunc(m.Media, func(md feature.Media) bool { return md.Pack != "" }) {
		text = strings.TrimSpace(text + " [sticker]")
	}
	if text == "" {
		return ""
	}
	return cmp.Or(m.Name, "someone") + ": " + text
}

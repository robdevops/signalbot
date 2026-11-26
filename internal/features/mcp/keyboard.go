package mcp

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/robdevops/signalbot/internal/feature"
)

// The limits of the buttons and keys the bot makes, Telegram's: rows, and
// buttons or keys in a row; a label's size in characters, a button's
// data's in bytes, and a key's command's in characters. keysPerRow is
// how many of the configured keys share a row.
const (
	maxRows    = 5
	maxPerRow  = 5
	maxLabel   = 32
	maxData    = 64
	maxCommand = 256
	keysPerRow = 3
)

// label reports whether s is a button's or key's label: 1 to maxLabel
// characters, with no space around them, as Telegram may trim a key's.
func label(s string) bool {
	return s != "" && strings.TrimSpace(s) == s && utf8.RuneCountInString(s) <= maxLabel
}

// checkKeyboard is why keys, a server's "keyboard", can't be one: pairs of
// a label and the command it sends.
func checkKeyboard(keys [][]string) string {
	if len(keys) > maxRows*keysPerRow {
		return fmt.Sprintf("keyboard has more than %d keys", maxRows*keysPerRow)
	}
	for _, k := range keys {
		if len(k) != 2 || !label(k[0]) || !strings.HasPrefix(k[1], "/") || len(k[1]) < 2 ||
			utf8.RuneCountInString(k[1]) > maxCommand || strings.ContainsAny(k[1], "\r\n") {
			return fmt.Sprintf("keyboard key %q isn't a label (up to %d characters) and a command, such as [\"Status\", \"/status\"]", strings.Join(k, ", "), maxLabel)
		}
	}
	return ""
}

// keyboard registers /keyboard, which shows the servers' keys as a DM's
// reply keyboard, and /keyboard off, which takes it away, where the bot is
// on a network that has one (in a chat on another, it says so). A label in two servers' keyboards is kept once, the
// first's, in their names' order.
func keyboard(h feature.Host, servers map[string]server) {
	if !h.Info().Caps.Keyboard {
		return
	}
	var keys []feature.Key
	seen := map[string]bool{}
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		for _, k := range servers[name].Keyboard {
			if !seen[k[0]] && len(keys) < maxRows*keysPerRow {
				seen[k[0]] = true
				keys = append(keys, feature.Key{Label: k[0], Command: k[1]})
			}
		}
	}
	if len(keys) == 0 {
		return
	}
	rows := slices.Collect(slices.Chunk(keys, keysPerRow))
	err := h.Command("keyboard", func(_ context.Context, m feature.Message, args string) feature.Action {
		switch a := strings.ToLower(strings.TrimSpace(args)); {
		case m.Chat.GroupID != "":
			return feature.Action{Answer: "The keyboard is for DMs."}
		case !h.Net(m.Chat).Caps.Keyboard:
			return feature.Action{Answer: "This chat app has no keyboard of buttons."}
		case a == "off":
			return feature.Action{Answer: "Keyboard off. /keyboard brings it back.", Markup: feature.Markup{NoKeyboard: true}}
		case a != "":
			return feature.Action{Answer: "Use /keyboard, or /keyboard off."}
		}
		return feature.Action{Answer: "Here are the buttons. /keyboard off hides them.", Markup: feature.Markup{Keyboard: rows}}
	})
	if err == nil {
		h.ListCommands("Keyboard", "keyboard")
	}
}

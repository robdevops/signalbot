package slack

import (
	"strings"
	"testing"
)

// The emoji table goes both ways, by any of Slack's names, ignoring
// variation selectors, with one skin tone named as Slack names it.
func TestEmojiTable(t *testing.T) {
	if len(emojiNames) < 1800 || len(emojiByBare) < 1800 {
		t.Fatalf("%d names, %d emoji", len(emojiNames), len(emojiByBare))
	}
	for name, want := range map[string]string{
		"+1": "👍", "thumbsup": "👍", "thumbsup::skin-tone-2": "👍", "heart": "❤️", "thinking_face": "🤔", "laughing": "😆",
		"smile": "😄", "rolling_on_the_floor_laughing": "🤣", "flag-au": "🇦🇺", "woman-facepalming": "🤦‍♀️",
		"parrot": "🦜", "custom_party": "", "": "", "::skin-tone-2": "",
	} {
		if got := emojiOf(name); got != want {
			t.Errorf("emojiOf(%q) = %q, want %q", name, got, want)
		}
	}
	for emoji, want := range map[string]string{
		"👍": "+1", "❤": "heart", "❤️": "heart", "🤔": "thinking_face", "🇦🇺": "flag-au", "🤦‍♀️": "woman-facepalming", "🤦‍♀": "woman-facepalming",
		"👍🏽": "+1::skin-tone-4", "👋🏿": "wave::skin-tone-6", "🧑🏻‍🤝‍🧑🏿": "people_holding_hands",
		"x": "", ":party_parrot:": "", "👍👍": "",
	} {
		if got := nameOfEmoji(emoji); got != want {
			t.Errorf("nameOfEmoji(%q) = %q, want %q", emoji, got, want)
		}
	}
	for line := range strings.Lines(emojiTable) { // every emoji to a name, and its first name back
		f := strings.Fields(line)
		if f[0] == "//" {
			continue
		}
		if nameOfEmoji(f[0]) == "" || emojiOf(f[1]) != f[0] {
			t.Fatalf("%q", line)
		}
	}
}

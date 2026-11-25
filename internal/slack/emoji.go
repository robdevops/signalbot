package slack

import (
	_ "embed"
	"strconv"
	"strings"
)

// emojiTable is emoji.txt: each emoji and Slack's names for it, the first
// the one it prefers, a line each. emoji_gen.go makes it from emoji.json of
// iamcal/emoji-data (https://github.com/iamcal/emoji-data, MIT licence,
// Copyright (c) 2013 Cal Henderson), the data Slack's own names come from.
//
//go:generate sh -c "curl -sSf https://raw.githubusercontent.com/iamcal/emoji-data/master/emoji.json | go run emoji_gen.go > emoji.txt"
//go:embed emoji.txt
var emojiTable string

// emojiNames is the emoji each of Slack's names stands for, and emojiByBare
// the name Slack prefers for each emoji, by the emoji without variation
// selectors. A reaction not in them is skipped, and an emoji not in them
// is sent as a message.
var emojiNames, emojiByBare = emojiMaps()

func emojiMaps() (names, byBare map[string]string) {
	names, byBare = map[string]string{}, map[string]string{}
	for line := range strings.Lines(emojiTable) {
		f := strings.Fields(line)
		if len(f) < 2 || f[0] == "//" {
			continue
		}
		for _, n := range f[1:] {
			names[n] = f[0]
		}
		if b := bare(f[0]); byBare[b] == "" {
			byBare[b] = f[1]
		}
	}
	return names, byBare
}

// bare is emoji without its variation selectors.
func bare(emoji string) string {
	return strings.ReplaceAll(emoji, "️", "")
}

// emojiOf is the emoji a reaction's name stands for, without any skin tone
// ("+1::skin-tone-2" is 👍), or "".
func emojiOf(name string) string {
	name, _, _ = strings.Cut(name, "::")
	return emojiNames[name]
}

// nameOfEmoji is emoji's name, ignoring variation selectors, or "". One
// skin tone is named as Slack names it ("+1::skin-tone-2"); more than one
// is dropped.
func nameOfEmoji(emoji string) string {
	tone, tones := 0, 0
	b := strings.Map(func(r rune) rune {
		if r >= 0x1f3fb && r <= 0x1f3ff { // the five skin tones, Slack's skin-tone-2 to 6
			tone, tones = int(r-0x1f3fb)+2, tones+1
			return -1
		}
		return r
	}, bare(emoji))
	name := emojiByBare[b]
	if name != "" && tones == 1 {
		name += "::skin-tone-" + strconv.Itoa(tone)
	}
	return name
}

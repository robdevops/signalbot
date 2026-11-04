package txt

import "strings"

// SingleEmoji reports whether s, trimmed, is exactly one emoji: a flag,
// a keycap (such as 1️⃣), or one pictograph with any variation selector,
// skin tone and tags, or several joined by zero-width joiners (👨‍👩‍👧).
// A symbol that is text unless asked otherwise, such as © or ™, counts
// only with the emoji variation selector (U+FE0F).
func SingleEmoji(s string) bool {
	r := []rune(strings.TrimSpace(s))
	switch {
	case len(r) == 0:
		return false
	case len(r) == 2 && regional(r[0]) && regional(r[1]):
		return true
	case keycap(r):
		return true
	}
	for i := 0; ; {
		if i >= len(r) || !pictograph(r[i]) {
			return false
		}
		textual := textDefault(r[i])
		i++
		for i < len(r) && (r[i] == 0xFE0F || skinTone(r[i]) || tag(r[i])) {
			if r[i] == 0xFE0F {
				textual = false
			}
			i++
		}
		if textual {
			return false
		}
		if i == len(r) {
			return true
		}
		if r[i] != 0x200D { // anything but a joiner starts a second emoji
			return false
		}
		i++
	}
}

func regional(r rune) bool { return r >= 0x1F1E6 && r <= 0x1F1FF }
func skinTone(r rune) bool { return r >= 0x1F3FB && r <= 0x1F3FF }
func tag(r rune) bool      { return r >= 0xE0020 && r <= 0xE007F }

// keycap is a digit, # or * with an optional U+FE0F, then U+20E3.
func keycap(r []rune) bool {
	if len(r) < 2 || !(r[0] >= '0' && r[0] <= '9' || r[0] == '#' || r[0] == '*') {
		return false
	}
	rest := r[1:]
	if rest[0] == 0xFE0F {
		rest = rest[1:]
	}
	return len(rest) == 1 && rest[0] == 0x20E3
}

// pictograph is a code point that can be an emoji on its own.
func pictograph(r rune) bool {
	switch {
	case r >= 0x1F000 && r <= 0x1FAFF: // mahjong to symbols and pictographs extended-A
		return !regional(r) && !skinTone(r)
	case r >= 0x2190 && r <= 0x21FF, r >= 0x2300 && r <= 0x23FF, r >= 0x25A0 && r <= 0x27BF,
		r >= 0x2900 && r <= 0x297F, r >= 0x2B00 && r <= 0x2BFF:
		return true
	}
	switch r {
	case 0x00A9, 0x00AE, 0x203C, 0x2049, 0x2122, 0x2139, 0x3030, 0x303D, 0x3297, 0x3299:
		return true
	}
	return false
}

// textDefault is a pictograph shown as text unless followed by U+FE0F.
func textDefault(r rune) bool {
	switch r {
	case 0x00A9, 0x00AE, 0x203C, 0x2049, 0x2122, 0x2139, 0x3030, 0x303D, 0x3297, 0x3299:
		return true
	}
	return r >= 0x2190 && r <= 0x21FF || r >= 0x2900 && r <= 0x297F
}

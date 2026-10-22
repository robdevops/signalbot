package main

import (
	"fmt"
	"sort"
	"unicode"
)

// atSpan is one "@name" found in a text, as rune indices [start, end).
type atSpan struct {
	start, end int
	uuid       string
}

// findAtNames finds "@Name" for each known name in rs. Matching is
// case-insensitive, longest name first, and must sit on word boundaries.
func findAtNames(rs []rune, known map[string]string) []atSpan {
	type cand struct {
		runes []rune
		uuid  string
	}
	cands := make([]cand, 0, len(known))
	for name, uuid := range known {
		if name != "" {
			cands = append(cands, cand{[]rune(name), uuid})
		}
	}
	if len(cands) == 0 {
		return nil
	}
	sort.Slice(cands, func(i, j int) bool { return len(cands[i].runes) > len(cands[j].runes) })

	var out []atSpan
	for i := 0; i < len(rs); i++ {
		if rs[i] != '@' || (i > 0 && isWordRune(rs[i-1])) {
			continue
		}
		for _, c := range cands {
			end := i + 1 + len(c.runes)
			if end > len(rs) || (end < len(rs) && isWordRune(rs[end])) || !equalFoldRunes(rs[i+1:end], c.runes) {
				continue
			}
			out = append(out, atSpan{i, end, c.uuid})
			i = end - 1
			break
		}
	}
	return out
}

// buildMentions returns signal-cli mention specs ("start:length:uuid") for the
// known @names in text. Offsets are UTF-16 code units. An ambiguous name
// (empty uuid) stays plain text.
func buildMentions(text string, known map[string]string) []string {
	rs := []rune(text)
	var out []string
	u, pos := 0, 0 // UTF-16 offset of rs[pos]
	for _, s := range findAtNames(rs, known) {
		u += utf16Len(rs[pos:s.start])
		length := utf16Len(rs[s.start:s.end])
		if s.uuid != "" {
			out = append(out, fmt.Sprintf("%d:%d:%s", u, length, s.uuid))
		}
		u += length
		pos = s.end
	}
	return out
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }

func utf16Len(rs []rune) int {
	n := 0
	for _, r := range rs {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

func equalFoldRunes(a, b []rune) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if unicode.ToLower(a[i]) != unicode.ToLower(b[i]) {
			return false
		}
	}
	return true
}

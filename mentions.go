package main

import (
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
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
		lower string
		n     int // runes
		uuid  string
	}
	cands := make([]cand, 0, len(known))
	for name, uuid := range known {
		if name != "" {
			cands = append(cands, cand{strings.ToLower(name), utf8.RuneCountInString(name), uuid})
		}
	}
	if len(cands) == 0 {
		return nil
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].n > cands[j].n })

	var out []atSpan
	for i := 0; i < len(rs); i++ {
		if rs[i] != '@' || (i > 0 && isWordRune(rs[i-1])) {
			continue
		}
		for _, c := range cands {
			end := i + 1 + c.n
			if end > len(rs) || (end < len(rs) && isWordRune(rs[end])) || strings.ToLower(string(rs[i+1:end])) != c.lower {
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

func utf16Len(rs []rune) (n int) {
	for _, r := range rs {
		n += utf16.RuneLen(r)
	}
	return n
}

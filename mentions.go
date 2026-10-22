package main

import (
	"fmt"
	"sort"
	"unicode"
)

// buildMentions finds "@Name" for each known name in text and returns signal-cli
// --mention specs ("start:length:uuid"). Offsets are UTF-16 code units. Matching is
// case-insensitive, longest name first, and must end on a word boundary.
func buildMentions(text string, known map[string]string) []string {
	if len(known) == 0 {
		return nil
	}
	type cand struct {
		runes []rune
		uuid  string
	}
	cands := make([]cand, 0, len(known))
	for name, uuid := range known {
		if name != "" && uuid != "" {
			cands = append(cands, cand{[]rune(name), uuid})
		}
	}
	sort.Slice(cands, func(i, j int) bool { return len(cands[i].runes) > len(cands[j].runes) })

	rs := []rune(text)
	var out []string
	u := 0 // UTF-16 offset of rs[i]
	for i := 0; i < len(rs); {
		matched := false
		if rs[i] == '@' && (i == 0 || !isWordRune(rs[i-1])) {
			for _, c := range cands {
				end := i + 1 + len(c.runes)
				if end > len(rs) || (end < len(rs) && isWordRune(rs[end])) || !equalFoldRunes(rs[i+1:end], c.runes) {
					continue
				}
				length := 1 + utf16Len(rs[i+1:end])
				out = append(out, fmt.Sprintf("%d:%d:%s", u, length, c.uuid))
				u += length
				i = end
				matched = true
				break
			}
		}
		if !matched {
			u += utf16Len(rs[i : i+1])
			i++
		}
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

package txt

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Span is one "@name" found in a text, as rune indices [Start, End).
type Span struct {
	Start, End int
	ID         string
}

// AtNames finds "@Name" for each known name in rs. Matching is
// case-insensitive, longest name first, and must sit on word boundaries.
func AtNames(rs []rune, known map[string]string) []Span {
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

	var out []Span
	for i := 0; i < len(rs); i++ {
		if rs[i] != '@' || (i > 0 && IsWordRune(rs[i-1])) {
			continue
		}
		for _, c := range cands {
			end := i + 1 + c.n
			if end > len(rs) || (end < len(rs) && IsWordRune(rs[end])) || strings.ToLower(string(rs[i+1:end])) != c.lower {
				continue
			}
			out = append(out, Span{i, end, c.uuid})
			i = end - 1
			break
		}
	}
	return out
}

// IsWordRune reports whether r can be part of a name: a letter, digit or _.
func IsWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }

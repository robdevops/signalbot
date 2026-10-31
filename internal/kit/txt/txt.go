// Package txt holds small helpers for untrusted text.
package txt

import (
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxNameRunes caps any display name spliced into a prompt or a log line.
const MaxNameRunes = 64

// Safe makes an untrusted display name safe to splice into a prompt or a log
// line: no newlines, no runs of whitespace, bounded length. Signal profile
// names are chosen by their owner, so they are attacker-controlled.
func Safe(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > MaxNameRunes {
		s = strings.TrimSpace(string(r[:MaxNameRunes]))
	}
	return s
}

// Clamp cuts s to max runes, marking a cut with "…".
func Clamp(s string, max int) string {
	if r := []rune(s); len(r) > max {
		return string(r[:max]) + "…"
	}
	return s
}

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

// Split breaks a reply into chunks of at most max runes, preferring a
// newline and then a space as the break.
func Split(s string, max int) []string {
	r := []rune(s)
	var out []string
	for {
		for len(r) > 0 && unicode.IsSpace(r[0]) {
			r = r[1:]
		}
		if len(r) == 0 {
			return out
		}
		end := min(len(r), max)
		if end < len(r) {
			if i := lastIndexRune(r[:end], '\n'); i > 0 {
				end = i
			} else if i := lastIndexRune(r[:end], ' '); i > max/2 {
				end = i
			}
		}
		out = append(out, strings.TrimRightFunc(string(r[:end]), unicode.IsSpace))
		r = r[end:]
	}
}

func lastIndexRune(r []rune, want rune) int {
	for i := len(r) - 1; i >= 0; i-- {
		if r[i] == want {
			return i
		}
	}
	return -1
}

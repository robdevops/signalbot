// Package txt makes untrusted text safe to splice into a prompt or a log
// line.
package txt

import "strings"

// MaxName caps a display name, in runes.
const MaxName = 64

// SafeName makes an untrusted name safe to splice into a prompt or a log
// line: no line breaks, no runs of white space, at most MaxName runes.
// Signal profile names are chosen by their owner, so anyone can set one.
func SafeName(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > MaxName {
		s = strings.TrimSpace(string(r[:MaxName]))
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

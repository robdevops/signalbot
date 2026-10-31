// Package txt holds small helpers for untrusted text.
package txt

import "strings"

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

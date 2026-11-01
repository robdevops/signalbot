// Package txt makes untrusted text safe to splice into a prompt or a log
// line, and reads settings written as text.
package txt

import "strings"

// maxName caps a display name, in runes.
const maxName = 64

// SafeName makes an untrusted name safe to splice into a prompt or a log
// line: no line breaks, no runs of white space, at most maxName runes.
// Display names are chosen by their owner, so anyone can set one.
func SafeName(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxName {
		s = strings.TrimSpace(string(r[:maxName]))
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

// Bool reads a yes/no setting: true, yes, on, 1 (or t, y), and their
// opposites, in any case. Anything else is def.
func Bool(s string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	case "0", "f", "false", "n", "no", "off":
		return false
	}
	return def
}

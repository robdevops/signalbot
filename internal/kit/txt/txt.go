// Package txt makes untrusted text safe to splice into a prompt or a log
// line, and reads settings written as text.
package txt

import (
	"strconv"
	"strings"
)

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

// Fence is text in a Markdown code block, cut at max runes with
// "[…truncated]", its fence longer than any run of backticks in it so the
// text can't close it early.
func Fence(text string, max int) string {
	if i := cutAt(text, max); i >= 0 {
		text = text[:i] + "\n[…truncated]"
	}
	fence := "```"
	for strings.Contains(text, fence) {
		fence += "`"
	}
	return fence + "\n" + strings.Trim(text, "\n") + "\n" + fence
}

// cutAt is the byte offset of the max-th rune of s, or -1 if s has no more
// than max runes: where to cut it, found without copying it into runes.
func cutAt(s string, max int) int {
	n := 0
	for i := range s {
		if n == max {
			return i
		}
		n++
	}
	return -1
}

// Chars says how much of n characters of text a cap of max shows, for a
// log line: "1532 chars of text", or "50000 of 83210 chars of text".
func Chars(n, max int) string {
	if n > max {
		return strconv.Itoa(max) + " of " + strconv.Itoa(n) + " chars of text"
	}
	return strconv.Itoa(n) + " chars of text"
}

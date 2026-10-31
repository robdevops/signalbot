package txt

import (
	"strings"
	"unicode"
)

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

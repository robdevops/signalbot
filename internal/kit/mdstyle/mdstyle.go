// Package mdstyle turns a reply's Markdown into plain text with each rune's
// styles, and links if kept, as chat networks that take styled text want:
// Signal's text styles, Telegram's entities. Offsets they need are UTF-16.
package mdstyle

import (
	"strings"
	"unicode"
)

// Style is a rune's text styles, as bits.
type Style uint8

// The styles.
const (
	Bold Style = 1 << iota
	Italic
	Strike
	Mono
	Spoiler
)

// Styles is every style, in bit order.
var Styles = [...]Style{Bold, Italic, Strike, Mono, Spoiler}

// delims are the inline markers, longest first, and their styles.
var delims = []struct {
	marker string
	style  Style
}{{"**", Bold}, {"__", Bold}, {"~~", Strike}, {"||", Spoiler}, {"*", Italic}, {"_", Italic}}

// Text is plain text with each rune's styles, and its links if kept.
type Text struct {
	Runes  []rune
	Styles []Style
	Links  []Link

	keepLinks bool // Parse's: links kept as Links
}

// Link is runes [Start, End) of a Text shown as a link to URL.
type Link struct {
	Start, End int
	URL        string
}

// Add appends rs, each with style st.
func (s *Text) Add(rs []rune, st Style) {
	s.Runes = append(s.Runes, rs...)
	for range rs {
		s.Styles = append(s.Styles, st)
	}
}

// Parse converts a reply's Markdown to plain text and styles. A fenced
// code block is its content in monospace, a "# Heading" its text in bold,
// and [text](url) its text kept as a Link if keepLinks, for a network that
// shows a link under its own text, else "text (url)"; lists, tables and
// quotes stay as written. Inline markers open and close only at word
// boundaries, an unmatched one stays literal, and nothing in code is
// converted.
func Parse(md string, keepLinks bool) Text {
	s := Text{}
	s.keepLinks = keepLinks
	lines := strings.Split(md, "\n")
	for i := 0; i < len(lines); i++ {
		if i > 0 {
			s.Add([]rune{'\n'}, 0)
		}
		line := lines[i]
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			if end := fenceEnd(lines, i+1); end > 0 {
				s.Add([]rune(strings.Join(lines[i+1:end], "\n")), Mono)
				i = end
				continue
			}
		}
		if h := strings.TrimLeft(line, "#"); len(line)-len(h) <= 6 && h != line && strings.HasPrefix(h, " ") {
			s.inline([]rune(strings.TrimSpace(h)), Bold)
			continue
		}
		s.inline([]rune(line), 0)
	}
	return s
}

// fenceEnd is the index of the line that closes a code block whose content
// starts at lines[from], or -1 if none does.
func fenceEnd(lines []string, from int) int {
	for i := from; i < len(lines); i++ {
		if t := strings.TrimSpace(lines[i]); strings.HasPrefix(t, "```") && strings.Trim(t, "`") == "" {
			return i
		}
	}
	return -1
}

// inline adds a line's text, converting its code spans, links and styled
// spans, with st as well.
func (s *Text) inline(rs []rune, st Style) {
	for i := 0; i < len(rs); {
		i += s.span(rs, i, st)
	}
}

// span adds the code span, link or styled span at rs[i], else the rune
// itself, and returns the runes it took.
func (s *Text) span(rs []rune, i int, st Style) int {
	switch rs[i] {
	case '`':
		n := repeats(rs, i)
		if end := codeEnd(rs, i+n, n); end >= 0 {
			s.Add(rs[i+n:end], st|Mono)
			return end + n - i
		}
		s.Add(rs[i:i+n], st) // unmatched, so literal
		return n
	case '[':
		if text, url, n := link(rs[i:]); n > 0 {
			switch {
			case string(text) == string(url):
				s.Add(url, st)
			case s.keepLinks:
				start := len(s.Runes)
				s.inline(text, st)
				s.Links = append(s.Links, Link{start, len(s.Runes), string(url)})
			default:
				s.inline(text, st)
				s.Add([]rune(" ("+string(url)+")"), st)
			}
			return n
		}
	}
	for _, d := range delims {
		c, n := rune(d.marker[0]), len(d.marker)
		if !opens(rs, i, c, n) {
			continue
		}
		if j := closes(rs, i+n, c, n); j > 0 {
			s.inline(rs[i+n:j], st|d.style)
			return j + n - i
		}
	}
	s.Add(rs[i:i+1], st)
	return 1
}

// repeats is how many times rs[i] repeats from i.
func repeats(rs []rune, i int) int {
	n := 1
	for i+n < len(rs) && rs[i+n] == rs[i] {
		n++
	}
	return n
}

// codeEnd is where a run of exactly n backticks starts at or after from,
// closing a code span, or -1.
func codeEnd(rs []rune, from, n int) int {
	for j := from; j < len(rs); j += repeats(rs, j) {
		if rs[j] == '`' && repeats(rs, j) == n {
			return j
		}
	}
	return -1
}

// link reads "[text](url)" at the start of rs, with a url without spaces,
// and returns the runes it took, or 0.
func link(rs []rune) (text, url []rune, n int) {
	k := 1
	for k < len(rs) && rs[k] != ']' {
		k++
	}
	if k < 2 || k+2 >= len(rs) || rs[k+1] != '(' || rs[k+2] == ')' {
		return nil, nil, 0
	}
	for m := k + 2; m < len(rs) && !unicode.IsSpace(rs[m]); m++ {
		if rs[m] == ')' {
			return rs[1:k], rs[k+2 : m], m + 1
		}
	}
	return nil, nil, 0
}

// opens reports whether n of c at rs[i] open a styled span: at the start
// of a word, followed by text.
func opens(rs []rune, i int, c rune, n int) bool {
	return rs[i] == c && repeats(rs, i) >= n && i+n < len(rs) && !unicode.IsSpace(rs[i+n]) && (i == 0 || !WordRune(rs[i-1]))
}

// closes is where n of c close a span whose text starts at rs[from], at the
// end of a word, skipping code spans; -1 if nowhere. In a run of three,
// "**" closes at its end, so "***x***" is bold and italic.
func closes(rs []rune, from int, c rune, n int) int {
	for j := from; j < len(rs); {
		l := repeats(rs, j)
		switch {
		case rs[j] == '`':
			if end := codeEnd(rs, j+l, l); end >= 0 {
				l = end + l - j
			}
		case rs[j] == c && (l == n || n == 2 && l == 3 && c != '~' && c != '|') && j > from &&
			!unicode.IsSpace(rs[j-1]) && (j+l == len(rs) || !WordRune(rs[j+l])):
			return j + l - n
		}
		j += l
	}
	return -1
}

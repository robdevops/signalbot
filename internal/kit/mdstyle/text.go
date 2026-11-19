package mdstyle

import (
	"maps"
	"slices"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// WordRune reports whether r is part of a word, for the word boundaries
// styles and @names sit on.
func WordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }

// UTF16At is the UTF-16 offset of each rune of rs, and then of its end.
func UTF16At(rs []rune) []int {
	u := make([]int, len(rs)+1)
	for i, r := range rs {
		u[i+1] = u[i] + utf16.RuneLen(r)
	}
	return u
}

// Run is a stretch of one style, in UTF-16 units, as networks give ranges.
type Run struct {
	Start, Len int
	Style      Style
}

// Runs is each run of each style in t, in style order.
func (t Text) Runs() []Run {
	u := UTF16At(t.Runes)
	var out []Run
	for _, st := range Styles {
		start := -1
		for i := range len(t.Runes) + 1 {
			on := i < len(t.Runes) && t.Styles[i]&st != 0
			if on && start < 0 {
				start = i
			} else if !on && start >= 0 {
				out = append(out, Run{u[start], u[i] - u[start], st})
				start = -1
			}
		}
	}
	return out
}

// Slice is runes [i, j) of t, its links clipped to them and rebased.
func (t Text) Slice(i, j int) Text {
	out := Text{Runes: t.Runes[i:j:j], Styles: t.Styles[i:j:j]}
	for _, l := range t.Links {
		if s, e := max(l.Start, i), min(l.End, j); s < e {
			out.Links = append(out.Links, Link{s - i, e - i, l.URL})
		}
	}
	return out
}

// Trim is t without white space at either end.
func (t Text) Trim() Text {
	i, j := 0, len(t.Runes)
	for i < j && unicode.IsSpace(t.Runes[i]) {
		i++
	}
	for j > i && unicode.IsSpace(t.Runes[j-1]) {
		j--
	}
	return t.Slice(i, j)
}

// Split cuts t into parts of at most max UTF-16 units, each trimmed: at the
// last line break past halfway, else the last space past halfway, else
// hard. A style or link carries on into the next part.
func Split(t Text, max int) []Text {
	var out []Text
	for t = t.Trim(); len(t.Runes) > 0; {
		u := UTF16At(t.Runes)
		end := len(t.Runes)
		for u[end] > max {
			end--
		}
		if end < len(t.Runes) {
			if i := lastBreak(t.Runes[:end], '\n', end/2); i > 0 {
				end = i
			} else if i := lastBreak(t.Runes[:end], ' ', end/2); i > 0 {
				end = i
			}
		}
		out = append(out, t.Slice(0, end).Trim())
		t = t.Slice(end, len(t.Runes)).Trim()
	}
	return out
}

// lastBreak is the index of the last r in rs past from, or -1.
func lastBreak(rs []rune, r rune, from int) int {
	for i := len(rs) - 1; i > from; i-- {
		if rs[i] == r {
			return i
		}
	}
	return -1
}

// Span is one "@name" in a text, as rune indices [Start, End), and the ID
// of who it names.
type Span struct {
	Start, End int
	ID         string
}

// FindAt finds "@name" for each name in known (name to ID). Matching
// ignores case, tries the longest name first, and sits on word boundaries.
func FindAt(rs []rune, known map[string]string) []Span {
	names := slices.DeleteFunc(slices.Collect(maps.Keys(known)), func(name string) bool { return name == "" })
	slices.SortFunc(names, func(a, b string) int { return utf8.RuneCountInString(b) - utf8.RuneCountInString(a) })
	var out []Span
	for i := 0; i < len(rs); i++ {
		if rs[i] != '@' || (i > 0 && WordRune(rs[i-1])) {
			continue
		}
		for _, name := range names {
			end := i + 1 + utf8.RuneCountInString(name)
			if end > len(rs) || (end < len(rs) && WordRune(rs[end])) || strings.ToLower(string(rs[i+1:end])) != strings.ToLower(name) {
				continue
			}
			out = append(out, Span{i, end, known[name]})
			i = end - 1
			break
		}
	}
	return out
}

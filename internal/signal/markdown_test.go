package signal

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/robdevops/signalbot/internal/feature"
)

func TestMarkdown(t *testing.T) {
	for _, tc := range []struct {
		md, text string
		styles   []string
	}{
		{"plain words", "plain words", nil},
		{"a **bold** word", "a bold word", []string{"2:4:BOLD"}},
		{"a __bold__ word", "a bold word", []string{"2:4:BOLD"}},
		{"an *italic* and _italic_", "an italic and italic", []string{"3:6:ITALIC", "14:6:ITALIC"}},
		{"~~gone~~ and ||secret||", "gone and secret", []string{"0:4:STRIKETHROUGH", "9:6:SPOILER"}},
		{"run `go test` now", "run go test now", []string{"4:7:MONOSPACE"}},
		{"***both***", "both", []string{"0:4:BOLD", "0:4:ITALIC"}},
		{"*an **inner** one*", "an inner one", []string{"3:5:BOLD", "0:12:ITALIC"}},
		{"**bold with `code`**", "bold with code", []string{"0:14:BOLD", "10:4:MONOSPACE"}},
		// Word boundaries and unmatched markers.
		{"2*3*4", "2*3*4", nil},
		{"snake_case_name", "snake_case_name", nil},
		{"a * b * c", "a * b * c", nil},
		{"**not closed", "**not closed", nil},
		{"one `tick", "one `tick", nil},
		{"****", "****", nil},
		{"**bold**text", "**bold**text", nil},
		// Nothing in code is converted.
		{"`**raw** _x_`", "**raw** _x_", []string{"0:11:MONOSPACE"}},
		{"*a `b*` c*", "a b* c", []string{"0:6:ITALIC", "2:2:MONOSPACE"}},
		{"```go\nx := **y**\n_z_\n```\nafter", "x := **y**\n_z_\nafter", []string{"0:14:MONOSPACE"}},
		{"```\nnot closed", "```\nnot closed", nil},
		// Headings, links, and what stays as written.
		{"# Title\nbody", "Title\nbody", []string{"0:5:BOLD"}},
		{"### A *b*", "A b", []string{"0:3:BOLD", "2:1:ITALIC"}},
		{"####### seven", "####### seven", nil},
		{"#hashtag", "#hashtag", nil},
		{"see [the docs](https://x.org/a_b_c).", "see the docs (https://x.org/a_b_c).", nil},
		{"[https://x.org](https://x.org)", "https://x.org", nil},
		{"[**b**](https://x.org)", "b (https://x.org)", []string{"0:1:BOLD"}},
		{"[a](no url)", "[a](no url)", nil},
		{"- one\n* two\n> quote\n| a | b |", "- one\n* two\n> quote\n| a | b |", nil},
		// UTF-16 offsets.
		{"\U0001F389 **yay** \U0001F389", "\U0001F389 yay \U0001F389", []string{"3:3:BOLD"}},
		{"**\U0001F389**", "\U0001F389", []string{"0:2:BOLD"}},
	} {
		s := markdown(tc.md)
		if got := string(s.text); got != tc.text {
			t.Errorf("%q: text %q, want %q", tc.md, got, tc.text)
		}
		if got := textStyles(s); !reflect.DeepEqual(got, tc.styles) {
			t.Errorf("%q: styles %v, want %v", tc.md, got, tc.styles)
		}
	}
}

// Styles survive the own-@name stripping, and a style across a split
// carries on into the next part, rebased and clipped to each.
func TestPartsKeepStyles(t *testing.T) {
	ps := parts("@Bot **hi** @Alice", "Bot")
	if got := texts(ps); !reflect.DeepEqual(got, []string{"hi @Alice"}) {
		t.Fatalf("got %q", got)
	}
	if got := textStyles(ps[0]); !reflect.DeepEqual(got, []string{"0:2:BOLD"}) {
		t.Errorf("styles %v", got)
	}
	long := "**" + strings.Repeat("a ", maxChars/2-3) + "\U0001F389 split here** after"
	ps = parts(long, "Bot")
	if len(ps) != 2 {
		t.Fatalf("%d parts", len(ps))
	}
	first, second := textStyles(ps[0]), textStyles(ps[1])
	if n := len(ps[0].text); !reflect.DeepEqual(first, []string{"0:" + strconv.Itoa(n+1) + ":BOLD"}) { // the emoji is two units
		t.Errorf("first part %v (%d runes)", first, n)
	}
	if string(ps[1].text) != "split here after" || !reflect.DeepEqual(second, []string{"0:10:BOLD"}) {
		t.Errorf("second part %q %v", string(ps[1].text), second)
	}
}

// Send passes the styles, and mentions land on the converted text.
func TestSendStyles(t *testing.T) {
	f := newFake(t)
	c := newClient(f.sock, nil, nil)
	run(t, c, f)
	c.Send(feature.Chat{User: aliceID}, "**\U0001F389 big** news, @Alice: see `x`", nil, map[string]string{"alice": "u-a"})
	eventually(t, "a send", func() bool { return len(f.calls("send")) == 1 })
	s := f.calls("send")[0]
	if s["message"] != "\U0001F389 big news, @Alice: see x" {
		t.Errorf("message %q", s["message"])
	}
	if got := fmt.Sprint(s["textStyle"]); got != "[0:6:BOLD 25:1:MONOSPACE]" {
		t.Errorf("styles %v", got)
	}
	if got := fmt.Sprint(s["mention"]); got != "[13:6:u-a]" {
		t.Errorf("mentions %v", got)
	}
	c.Send(feature.Chat{User: aliceID}, "plain", nil, nil)
	eventually(t, "a second send", func() bool { return len(f.calls("send")) == 2 })
	if _, ok := f.calls("send")[1]["textStyle"]; ok {
		t.Errorf("styles on plain text")
	}
}

package mdstyle

import (
	"reflect"
	"strings"
	"testing"
)

// A link is kept under its own text when asked, else written out after it;
// a link whose text is its URL is the URL either way.
func TestLinks(t *testing.T) {
	md := "see [the **docs**](https://x.example/d) or https://y.example and [https://z.example](https://z.example)"
	kept := Parse(md, true)
	if got := string(kept.Runes); got != "see the docs or https://y.example and https://z.example" {
		t.Fatalf("kept %q", got)
	}
	if want := []Link{{4, 12, "https://x.example/d"}}; !reflect.DeepEqual(kept.Links, want) {
		t.Fatalf("links %v", kept.Links)
	}
	if got := kept.Runs(); !reflect.DeepEqual(got, []Run{{8, 4, Bold}}) {
		t.Errorf("runs %v", got)
	}
	plain := Parse(md, false)
	if got := string(plain.Runes); got != "see the docs (https://x.example/d) or https://y.example and https://z.example" || plain.Links != nil {
		t.Fatalf("plain %q %v", got, plain.Links)
	}
}

// Runs count in UTF-16 units, so an emoji before a style moves it by two.
func TestRunsUTF16(t *testing.T) {
	if got := Parse("🎉 **hi**", false).Runs(); !reflect.DeepEqual(got, []Run{{3, 2, Bold}}) {
		t.Fatalf("runs %v", got)
	}
}

// Split keeps each part within max UTF-16 units, cuts at a line break or a
// space past halfway, else hard, and carries styles and links on.
func TestSplit(t *testing.T) {
	long := strings.Repeat("word ", 30) + "\n" + strings.Repeat("x", 40)
	parts := Split(Parse(long, false), 160)
	if len(parts) != 2 || string(parts[0].Runes) != strings.TrimSpace(strings.Repeat("word ", 30)) || string(parts[1].Runes) != strings.Repeat("x", 40) {
		t.Fatalf("at the line break: %q", texts(parts))
	}
	// A line break near the start is no place to cut: the last space past
	// halfway is.
	early := "a\n" + strings.Repeat("word ", 40)
	if parts := Split(Parse(early, false), 100); len([]rune(string(parts[0].Runes))) < 50 {
		t.Fatalf("cut at the early line break: %q", texts(parts))
	}
	if parts := Split(Parse(strings.Repeat("y", 250), false), 100); len(parts) != 3 || len(parts[0].Runes) != 100 {
		t.Fatalf("hard: %q", texts(parts))
	}
	// Emoji are two units each, so 60 fit in 120.
	if parts := Split(Parse(strings.Repeat("🎉", 100), false), 120); len(parts) != 2 || len(parts[0].Runes) != 60 {
		t.Fatalf("UTF-16: %d parts, first %d runes", len(parts), len(parts[0].Runes))
	}
	styled := Parse("**"+strings.TrimSpace(strings.Repeat("b ", 60))+"** [link text that spans](https://x.example)", true)
	parts = Split(styled, 100)
	for i, p := range parts {
		if u := UTF16At(p.Runes); u[len(p.Runes)] > 100 {
			t.Errorf("part %d is %d units", i, u[len(p.Runes)])
		}
	}
	if r := parts[0].Runs(); len(r) != 1 || r[0].Style != Bold || r[0].Start != 0 {
		t.Errorf("first part runs %v", r)
	}
	last := parts[len(parts)-1]
	if len(last.Links) != 1 || last.Links[0].URL != "https://x.example" || string(last.Runes[last.Links[0].Start:last.Links[0].End]) == "" {
		t.Errorf("last part links %v in %q", last.Links, string(last.Runes))
	}
	if Split(Parse("  \n ", false), 10) != nil {
		t.Error("blank gave parts")
	}
}

func TestFindAt(t *testing.T) {
	got := FindAt([]rune("hi @Ann and @ann marie, not x@ann"), map[string]string{"ann": "1", "ann marie": "2"})
	if want := []Span{{3, 7, "1"}, {12, 22, "2"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("spans %v", got)
	}
}

func texts(ps []Text) []string {
	var out []string
	for _, p := range ps {
		out = append(out, string(p.Runes))
	}
	return out
}

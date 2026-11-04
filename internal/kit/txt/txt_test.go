package txt

import (
	"strings"
	"testing"
)

func TestSafeName(t *testing.T) {
	if got := SafeName("  Alice \n\t Bob\r "); got != "Alice Bob" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("é", maxName+10)
	if got := SafeName(long); len([]rune(got)) != maxName {
		t.Errorf("not capped: %d runes", len([]rune(got)))
	}
}

func TestClamp(t *testing.T) {
	if got := Clamp("héllo", 3); got != "hél…" {
		t.Errorf("got %q", got)
	}
	if got := Clamp("hi", 3); got != "hi" {
		t.Errorf("got %q", got)
	}
}

func TestBool(t *testing.T) {
	for _, s := range []string{"1", "T", "true", "Yes", " on "} {
		if !Bool(s, false) {
			t.Errorf("%q is not true", s)
		}
	}
	for _, s := range []string{"0", "F", "FALSE", "no", "off"} {
		if Bool(s, true) {
			t.Errorf("%q is not false", s)
		}
	}
	if !Bool("", true) || Bool("maybe", false) {
		t.Error("an unknown value is not the default")
	}
}

func TestSingleEmoji(t *testing.T) {
	for s, want := range map[string]bool{
		"👍": true, " 🙏 ": true, "❤️": true, "❤": true, "👍🏽": true, "🇦🇺": true, "👨‍👩‍👧": true, "🏳️‍🌈": true,
		"1️⃣": true, "#⃣": true, "©️": true, "⭐": true, "☕": true, "🏴\U000E0067\U000E0062\U000E0065\U000E006E\U000E0067\U000E007F": true,
		"": false, "ok": false, "👍👍": false, "👍 thanks": false, "thanks 👍": false, "7": false, "©": false, "™": false,
		"↔": false, "↔️": true, "✓": false, "★": false, "■": false, "●": false, "✌": false, "✌️": true, "✅": true, "⚡": true, "♈": true, "🇦": false, "🇦🇺🇳🇿": false, "‍": false, "👍‍": false, "🏽": false, "1": false,
	} {
		if got := SingleEmoji(s); got != want {
			t.Errorf("SingleEmoji(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestFence(t *testing.T) {
	if got := Fence("a ``` b\n", 10); got != "````\na ``` b\n````" {
		t.Errorf("got %q", got)
	}
	if got := Fence("abcdef", 3); got != "```\nabc\n[…truncated]\n```" {
		t.Errorf("got %q", got)
	}
}

// Chars says how much of a text a cap shows.
func TestChars(t *testing.T) {
	if got := Chars(1532, 50_000); got != "1532 chars of text" {
		t.Errorf("got %q", got)
	}
	if got := Chars(83210, 50_000); got != "50000 of 83210 chars of text" {
		t.Errorf("got %q", got)
	}
}

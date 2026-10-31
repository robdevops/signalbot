package txt

import (
	"strings"
	"testing"
)

func TestSafeName(t *testing.T) {
	if got := SafeName("  Alice \n\t Bob\r "); got != "Alice Bob" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("é", MaxName+10)
	if got := SafeName(long); len([]rune(got)) != MaxName {
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

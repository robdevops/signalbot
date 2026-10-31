package txt

import (
	"strings"
	"testing"
	"time"
)

func TestSplitMessageIsLinear(t *testing.T) {
	big := strings.Repeat("word ", (4<<20)/5) // ~4 MiB, the response body cap
	start := time.Now()
	parts := Split(big, 2000)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("splitting 4 MiB took %v; it must be linear", took)
	}
	for i, p := range parts {
		if n := len([]rune(p)); n == 0 || n > 2000 {
			t.Fatalf("part %d has %d runes", i, n)
		}
	}
	if got := strings.Join(parts, " "); strings.Join(strings.Fields(got), " ") != strings.TrimSpace(big) {
		t.Fatal("splitting lost or reordered text")
	}
	if got := Split("   \n\t ", 10); len(got) != 0 {
		t.Fatalf("whitespace-only reply should produce no parts: %q", got)
	}
}

func TestClampKeepsRunesWhole(t *testing.T) {
	out := Clamp(strings.Repeat("é", 300), 201)
	if strings.ContainsRune(out, '\uFFFD') {
		t.Fatalf("clamp split a rune: %q", out)
	}
	if got := []rune(strings.TrimSuffix(out, "…")); len(got) != 201 {
		t.Fatalf("want 201 runes, got %d", len(got))
	}
}

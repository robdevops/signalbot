package bot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/feature"
)

// The notice rounds the time to midnight up to whole minutes, and from 60
// minutes up to whole hours.
func TestLimitNotice(t *testing.T) {
	for left, want := range map[time.Duration]string{
		5*time.Hour + time.Minute:       "6 hours",
		time.Hour:                       "1 hour",
		59*time.Minute + 30*time.Second: "1 hour",
		59 * time.Minute:                "59 minutes",
		25 * time.Minute:                "25 minutes",
		20 * time.Second:                "1 minute",
		0:                               "1 minute",
	} {
		if got := limitNotice(left); got != "I've reached today's spending limit for this chat; I'll be back in "+want+"." {
			t.Errorf("%v: %q", left, got)
		}
	}
}

// A chat over its limit is told once a day; spending survives a restart in
// budget.json, which holds no chat's ID, and starts again at midnight.
func TestBudget(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 4, 22, 30, 0, 0, time.Local)
	clock := func() time.Time { return now }
	b := newBudget(dir, 1, clock)
	const group, dm = "g:Z3JvdXA=", "u:+15551234567"
	if over, _, _ := b.over(group); over {
		t.Fatal("over before spending")
	}
	b.add(group, 0.6)
	b.add(group, -5) // ignored
	if over, _, _ := b.over(group); over {
		t.Fatal("over at 0.60")
	}
	b.add(group, 0.4)
	over, tell, left := b.over(group)
	if !over || !tell || left != 90*time.Minute {
		t.Fatalf("at 1.00: over %v, tell %v, left %v", over, tell, left)
	}
	if over, tell, _ := b.over(group); !over || tell {
		t.Fatalf("told twice: over %v, tell %v", over, tell)
	}
	if over, _, _ := b.over(dm); over || len(b.chats) != 1 {
		t.Fatalf("another chat is over, or kept: %d chats", len(b.chats))
	}
	data, err := os.ReadFile(filepath.Join(dir, "budget.json"))
	if err != nil || strings.Contains(string(data), "Z3JvdXA") || strings.Contains(string(data), "15551234567") || !strings.Contains(string(data), `"day": "2026-11-04"`) {
		t.Fatalf("budget.json %s, %v", data, err)
	}
	// After a restart, still over and already told.
	b = newBudget(dir, 1, clock)
	if over, tell, _ := b.over(group); !over || tell {
		t.Fatalf("after a restart: over %v, tell %v", over, tell)
	}
	// After midnight, a new day.
	now = now.Add(2 * time.Hour)
	if over, _, _ := b.over(group); over {
		t.Fatal("over the next day")
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "budget.json")); !strings.Contains(string(data), "\"day\": \"2026-11-05\",\n  \"chats\": {}") {
		t.Fatalf("yesterday's spending stayed on disk: %s", data)
	}
	// A file from another day, or broken, starts at 0.
	os.WriteFile(filepath.Join(dir, "budget.json"), []byte("{broken"), 0o600)
	if b := newBudget(dir, 1, clock); len(b.chats) != 0 {
		t.Fatal("a broken file was read")
	}
}

// Files sent with one message stop at maxFileBytes together; the rest are
// counted, in order.
func TestFitFiles(t *testing.T) {
	mb := func(n int) feature.File { return feature.File{Data: make([]byte, n<<20)} }
	fit, left := fitFiles([]feature.File{mb(10), mb(10), mb(5), mb(1)})
	if len(fit) != 3 || left != 1 || len(fit[1].Data) != 5<<20 {
		t.Fatalf("fit %d, left %d", len(fit), left)
	}
	if fit, left := fitFiles(nil); fit != nil || left != 0 {
		t.Fatal("nil")
	}
}

// Files left out by the cap are noted for the model and logged, as a
// warning, without their names.
func TestFilesOverTheCapAreLogged(t *testing.T) {
	h := start(t, options{features: func(b *Bot) {
		b.OnTurn(func(context.Context, feature.Message) feature.Additions {
			return feature.Additions{Files: []feature.File{{Name: "big.pdf", MIME: "application/pdf", Data: make([]byte, 9<<20)},
				{Name: "bigger.pdf", MIME: "application/pdf", Data: make([]byte, 8<<20)}}}
		})
	}})
	h.b.files.Store(true)
	r, _ := h.ask(dm("read these"))
	if c := contents(r); !strings.HasSuffix(c[len(c)-1], "\n[1 of the files attached left out: together they are over 16 MB]") {
		t.Fatalf("model got %q", c[len(c)-1])
	}
	if logs := h.logs.String(); strings.Count(logs, "<4>1 of the files left out: together they are over 16 MB\n") != 1 || strings.Contains(logs, "big") {
		t.Fatalf("log %q", logs)
	}
}

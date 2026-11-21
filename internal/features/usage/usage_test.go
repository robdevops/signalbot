package usage

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/feature/featuretest"
)

func f(v float64) *float64 { return &v }

func answer(t *testing.T, h *featuretest.Host, m feature.Message) string {
	t.Helper()
	return h.Message(context.Background(), m).Answer
}

// /usage and its aliases give this chat's day and the key's totals, in a
// DM and in a group alike.
func TestUsage(t *testing.T) {
	h := featuretest.New()
	h.Bot = feature.Info{Model: "x/model"}
	h.Use = feature.Usage{Tracked: true, Spent: 0.042, Limit: 1, Resets: 3*time.Hour + 10*time.Minute,
		Key: &feature.KeyUsage{Today: f(3.41), Week: f(12.8), Month: f(18.2), Total: f(40), Limit: f(100), Remaining: f(81.8), Reset: "monthly"}}
	if err := Register(h); err != nil {
		t.Fatal(err)
	}
	dm := feature.Message{Chat: feature.Chat{User: "alice"}, Sender: "alice", Addressed: true}
	want := "**This chat today:** $0.04 of $1.00 (4%) · resets in 3 hours\n" +
		"**Account:** $3.41 today · $12.80 this week · $18.20 this month · $40.00 all time · $81.80 of $100.00 left (resets monthly)"
	for _, cmd := range []string{"/usage", "/credits", "/TOKENS"} {
		dm.Text = cmd
		if got := answer(t, h, dm); got != want {
			t.Fatalf("%s:\n%s", cmd, got)
		}
	}
	group := feature.Message{Chat: feature.Chat{GroupID: "g"}, Sender: "alice", Addressed: true, Text: "/usage"}
	if got := answer(t, h, group); got != want {
		t.Fatalf("group:\n%s", got)
	}
	dm.Text = "/usage please explain"
	if a := h.Message(context.Background(), dm); a.Answer != "" {
		t.Fatalf("with more text: %q", a.Answer)
	}
}

// Without a daily budget the chat isn't counted; a key with no limit says
// so; another API says nothing of the key; with no model, nothing is spent.
func TestUsageWithout(t *testing.T) {
	h := featuretest.New()
	h.Bot = feature.Info{Model: "x/model"}
	h.Use = feature.Usage{Key: &feature.KeyUsage{Today: f(0.5)}}
	Register(h)
	dm := feature.Message{Chat: feature.Chat{User: "alice"}, Sender: "alice", Addressed: true, Text: "/usage"}
	if got := answer(t, h, dm); !strings.Contains(got, "not counted, as there's no daily budget") || !strings.Contains(got, "**Account:** $0.50 today\n") && !strings.HasSuffix(got, "**Account:** $0.50 today") {
		t.Fatalf("got:\n%s", got)
	}
	h.Use.Key = nil
	if got := answer(t, h, dm); !strings.Contains(got, "the API doesn't say what the key has spent") {
		t.Fatalf("got:\n%s", got)
	}
	h.Bot.Model = ""
	if got := answer(t, h, dm); got != "There's no model, so nothing is spent." {
		t.Fatalf("got %q", got)
	}
}

// Only the periods the API reports are shown; with none, it says so.
func TestPeriods(t *testing.T) {
	got := report(feature.Usage{Key: &feature.KeyUsage{Month: f(18.2), Limit: f(5), Remaining: f(1.25)}})
	if !strings.HasSuffix(got, "**Account:** $18.20 this month · $1.25 of $5.00 left") {
		t.Fatalf("got:\n%s", got)
	}
	if got := report(feature.Usage{Key: &feature.KeyUsage{}}); !strings.HasSuffix(got, "**Account:** no figures reported") {
		t.Fatalf("got:\n%s", got)
	}
}

func TestUntil(t *testing.T) {
	for d, want := range map[time.Duration]string{90 * time.Minute: "2 hours", 65 * time.Minute: "1 hour", 59 * time.Minute: "59 minutes", 0: "1 minute"} {
		if got := until(d); got != want {
			t.Errorf("%v: got %q, want %q", d, got, want)
		}
	}
}

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
	h.Use = feature.Usage{Tracked: true, Spent: 0.042, Limit: 1, In: 45210, Out: 3104, Resets: 3*time.Hour + 10*time.Minute,
		Key: &feature.KeyUsage{Today: 3.41, Week: 12.8, Month: 18.2, Total: 40, Limit: f(100), Remaining: f(81.8), Reset: "monthly"}}
	if err := Register(h); err != nil {
		t.Fatal(err)
	}
	dm := feature.Message{Chat: feature.Chat{User: "alice"}, Sender: "alice", Addressed: true}
	want := "**This chat today:** $0.042 of $1.00 (4%) · 45,210 tokens in, 3,104 out · resets in 3 hours\n" +
		"**Account (API key):** $3.41 today · $12.80 this week · $18.20 this month · $81.80 of $100.00 left (resets monthly)"
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
	h.Use = feature.Usage{Key: &feature.KeyUsage{Today: 0.5}}
	Register(h)
	dm := feature.Message{Chat: feature.Chat{User: "alice"}, Sender: "alice", Addressed: true, Text: "/usage"}
	if got := answer(t, h, dm); !strings.Contains(got, "not counted, as there's no daily budget") || !strings.Contains(got, "no credit limit on the key") {
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

func TestFormats(t *testing.T) {
	for _, c := range []struct {
		got, want string
	}{
		{dollars(0), "$0.00"}, {dollars(0.0004), "$0.000"}, {dollars(0.25), "$0.25"}, {dollars(12.3456), "$12.35"},
		{count(0), "0"}, {count(999), "999"}, {count(1000), "1,000"}, {count(1234567), "1,234,567"},
		{until(90 * time.Minute), "2 hours"}, {until(65 * time.Minute), "1 hour"}, {until(59 * time.Minute), "59 minutes"}, {until(0), "1 minute"},
	} {
		if c.got != c.want {
			t.Errorf("got %q, want %q", c.got, c.want)
		}
	}
}

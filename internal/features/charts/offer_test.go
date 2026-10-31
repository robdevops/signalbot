package charts

import (
	"testing"

	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/feature/featuretest"
)

func TestWantsChart(t *testing.T) {
	for text, want := range map[string]bool{
		"chart NVDA vs AMD": true, "Can you graph this?": true, "plot the yields": true,
		"visualise it": true, "VISUALIZATION please": true, "charts?": true, "plotted over time": true,
		"a pie chart": true, "donut please": true, "doughnut": true, "pied": false, "spied on": false,
		"what is up": false, "a plotless film": false, "paragraph": false,
	} {
		if got := wantsChart(text); got != want {
			t.Errorf("%q: got %v, want %v", text, got, want)
		}
	}
}

// After a message asks for a chart or a reply draws one, the chart tools stay
// on for 2 more replies in that chat, for follow-ups; never on a chat
// command's turn; and the count goes with the chat's history.
func TestChartFollowUps(t *testing.T) {
	f := &Feature{}
	h := featuretest.New(nil)
	f.Register(h)
	group := h.Groups[0]
	reply := func(c chat.ChatID, text string, drew bool) bool {
		turn := feature.Turn{Message: chat.Message{Chat: c, Text: text}}
		offered := group.Offer(turn)
		if offered {
			turn.Offered = []string{chartTool, donutTool}
		}
		var r feature.Reply
		if drew {
			r.Used = []string{chartTool}
		}
		h.Replies[0](turn, r)
		return offered
	}
	for i, step := range []struct {
		text string
		drew bool
		want bool
	}{
		{"chart NVDA for 3 months", true, true},
		{"show that for 1 month", false, true},
		{"and for a week", true, true}, // drew one: the count restarts
		{"thanks", false, true},
		{"what about AMD", false, true},
		{"ok", false, false},
	} {
		if got := reply("u:a", step.text, step.drew); got != step.want {
			t.Fatalf("step %d %q: chart tools %v, want %v", i, step.text, got, step.want)
		}
		if reply("u:other", "hello", false) {
			t.Fatal("another chat got the chart tools")
		}
	}
	reply("u:a", "plot it", false)
	if group.Offer(feature.Turn{Message: chat.Message{Chat: "u:a", Text: "plot it"}, Forced: "x__y"}) {
		t.Fatal("a chat command's turn got the chart tools")
	}
	h.Forgets[0]("u:a")
	if f.turns("u:a") != 0 {
		t.Fatal("erasing the history kept the chart tools on")
	}
}

// A reply with charts is sent as their captions, in place of the text.
func TestChartReplyIsItsCaption(t *testing.T) {
	f := &Feature{}
	h := featuretest.New(nil)
	f.Register(h)
	out := h.Replies[0](feature.Turn{Message: chat.Message{Chat: "c"}}, feature.Reply{Text: "lots of words",
		Pictures: []chat.Picture{{Caption: "NVDA, 5 years"}, {Caption: ""}}})
	if out == nil || out.Text != "NVDA, 5 years" {
		t.Fatalf("got %+v", out)
	}
	if h.Replies[0](feature.Turn{}, feature.Reply{Text: "no chart"}) != nil {
		t.Fatal("a reply without a chart was replaced")
	}
}

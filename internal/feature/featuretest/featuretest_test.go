package featuretest

import (
	"context"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/feature"
)

func TestHooksRunInOrder(t *testing.T) {
	h := New()
	h.OnMessage(func(context.Context, feature.Message) feature.Action { return feature.Action{} })
	h.OnMessage(func(context.Context, feature.Message) feature.Action { return feature.Action{Context: "first"} })
	h.OnMessage(func(context.Context, feature.Message) feature.Action { return feature.Action{Context: "second"} })
	if a := h.Message(context.Background(), feature.Message{}); a.Context != "first" {
		t.Errorf("got %+v", a)
	}
	h.OnTurn(func(context.Context, feature.Turn) feature.Additions { return feature.Additions{Note: "n"} })
	if adds := h.Turn(context.Background(), feature.Turn{}); len(adds) != 1 || adds[0].Note != "n" {
		t.Errorf("got %+v", adds)
	}
	h.OnReply(func(_ feature.Turn, r *feature.Reply) { r.Text += "!" })
	r := &feature.Reply{Text: "hi"}
	h.Reply(feature.Turn{}, r)
	if r.Text != "hi!" {
		t.Errorf("got %q", r.Text)
	}
}

// As in the core, only what applies to a message counts: Context on an
// addressed one, or Answer on an unaddressed one, doesn't win.
func TestActionsCountAsInTheCore(t *testing.T) {
	h := New()
	h.OnMessage(func(context.Context, feature.Message) feature.Action {
		return feature.Action{Context: "ctx", Answer: "early"}
	})
	h.OnMessage(func(context.Context, feature.Message) feature.Action { return feature.Action{Answer: "late"} })
	if a := h.Message(context.Background(), feature.Message{Addressed: true}); a != (feature.Action{Answer: "early"}) {
		t.Errorf("addressed: got %+v", a)
	}
	if a := h.Message(context.Background(), feature.Message{}); a != (feature.Action{Context: "ctx"}) {
		t.Errorf("unaddressed: got %+v", a)
	}
	h = New()
	h.OnMessage(func(context.Context, feature.Message) feature.Action { return feature.Action{Context: "ctx"} })
	h.OnMessage(func(context.Context, feature.Message) feature.Action { return feature.Action{Answer: "a"} })
	if a := h.Message(context.Background(), feature.Message{Addressed: true}); a.Answer != "a" {
		t.Errorf("Context won on an addressed message: %+v", a)
	}
}

func TestTimers(t *testing.T) {
	h := New()
	now := time.Now()
	var got []time.Time
	fn := func(_ context.Context, due time.Time) { got = append(got, due) }
	h.At("a", now.Add(time.Hour), fn)
	h.At("a", now, fn)
	cancel, _ := h.At("b", now, fn)
	cancel()
	if n := len(h.Timers()); n != 2 {
		t.Fatalf("%d timers", n)
	}
	if !h.Fire(context.Background(), "a") || len(got) != 1 || !got[0].Equal(now) {
		t.Errorf("fired %v", got)
	}
	if h.Fire(context.Background(), "b") {
		t.Error("a cancelled timer ran")
	}
}

func TestEnvRecorded(t *testing.T) {
	h := New()
	h.Settings["X"] = " 1 "
	if h.Env("X", "d") != "1" || h.Env("Y", "d") != "d" {
		t.Error("wrong values")
	}
	if r := h.EnvRead(); len(r) != 2 || r[0] != "X" || r[1] != "Y" {
		t.Errorf("read %v", r)
	}
}

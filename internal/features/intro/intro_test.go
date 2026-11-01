package intro

import (
	"context"
	"testing"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/feature/featuretest"
)

// /intro sends the bot's introduction, in any case, without the model;
// anything after it carries on.
func TestIntro(t *testing.T) {
	h := featuretest.New()
	h.Bot.Intro = "hi, I'm Botty (tools: **charts**)."
	if err := Register(h); err != nil {
		t.Fatal(err)
	}
	want := feature.Action{Answer: "hi, I'm Botty (tools: **charts**).\n\nThis message did not use the model."}
	for _, text := range []string{"/intro", "/INTRO"} {
		m := feature.Message{Chat: feature.Chat{User: "u-alice"}, Sender: "u-alice", Name: "Alice", Text: text, Addressed: true}
		if a := h.Message(context.Background(), m); a != want {
			t.Fatalf("%s: got %+v", text, a)
		}
	}
	m := feature.Message{Chat: feature.Chat{User: "u-alice"}, Sender: "u-alice", Text: "/intro yourself", Addressed: true}
	if a := h.Message(context.Background(), m); a != (feature.Action{}) {
		t.Fatalf("with args got %+v", a)
	}
}

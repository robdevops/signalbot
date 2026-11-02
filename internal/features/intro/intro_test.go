package intro

import (
	"context"
	"testing"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/feature/featuretest"
)

// /intro and /help send the bot's introduction, in any case, without the
// model: a DM's in a DM, a group's in a group. Anything after carries on.
func TestIntro(t *testing.T) {
	h := featuretest.New()
	h.Bot.Intro, h.Bot.IntroDM = "hi, I'm Botty (tools: **charts**).", "hi, I'm Botty. DM me."
	if err := Register(h); err != nil {
		t.Fatal(err)
	}
	dm := feature.Chat{User: "u-alice"}
	group := feature.Chat{GroupID: "g1"}
	for _, text := range []string{"/intro", "/INTRO", "/help", "/Help"} {
		for chat, want := range map[feature.Chat]string{dm: "hi, I'm Botty. DM me.", group: "hi, I'm Botty (tools: **charts**)."} {
			m := feature.Message{Chat: chat, Sender: "u-alice", Name: "Alice", Text: text, Addressed: true}
			if a := h.Message(context.Background(), m); a != (feature.Action{Answer: want + "\n\nThis message did not use the model."}) {
				t.Fatalf("%s in %+v: got %+v", text, chat, a)
			}
		}
	}
	for _, text := range []string{"/intro yourself", "/help me"} {
		m := feature.Message{Chat: dm, Sender: "u-alice", Text: text, Addressed: true}
		if a := h.Message(context.Background(), m); a != (feature.Action{}) {
			t.Fatalf("%s got %+v", text, a)
		}
	}
}

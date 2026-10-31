package example

import (
	"context"
	"strings"
	"testing"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/feature/featuretest"
)

func host(t *testing.T, tools ...string) *featuretest.Host {
	t.Helper()
	h := featuretest.New()
	h.Bot = feature.Info{Name: "Botty", Model: "x/test-model", CallsTools: len(tools) > 0, Tools: tools}
	if err := Register(h); err != nil {
		t.Fatal(err)
	}
	return h
}

// "/example" is answered without the model, greeting the sender by @name in
// a group so the core can link a real mention.
func TestExample(t *testing.T) {
	h := host(t, "web search", "charts", "get_forecast")
	group := feature.Message{Chat: feature.Chat{GroupID: "g1"}, Sender: "u-alice", Name: "Alice", Text: "/example", Addressed: true}
	want := "Hi @Alice! I'm Botty. Here's what I can do:\n" +
		"- Answer when you @mention me, reply to one of my messages, or DM me (if you're on my list)\n" +
		"- Use a message you quote as context\n" +
		"- Remember our conversation for up to a day (less with disappearing messages)\n" +
		"- Mention people by name in my replies\n" +
		"- Run test-model with web search, charts, get_forecast\n" +
		"This reply didn't use the model."
	if a := h.Message(context.Background(), group); a != (feature.Action{Answer: want}) {
		t.Fatalf("got %+v", a)
	}

	// In a DM, or with no name, there is no one to mention.
	dm := feature.Message{Chat: feature.Chat{User: "u-alice"}, Sender: "u-alice", Name: "Alice", Text: "/EXAMPLE", Addressed: true}
	if a := h.Message(context.Background(), dm); !strings.HasPrefix(a.Answer, "Hi! I'm Botty. ") {
		t.Fatalf("DM got %q", a.Answer)
	}
	group.Name = ""
	if a := h.Message(context.Background(), group); !strings.HasPrefix(a.Answer, "Hi! I'm Botty. ") {
		t.Fatalf("no name got %q", a.Answer)
	}
}

func TestExampleWithoutTools(t *testing.T) {
	h := host(t)
	m := feature.Message{Chat: feature.Chat{User: "u-bob"}, Text: "/example", Addressed: true}
	if a := h.Message(context.Background(), m); !strings.Contains(a.Answer, "- Run test-model with no tools\n") {
		t.Fatalf("got %q", a.Answer)
	}
}

// Only "/example" itself, addressed, is answered; anything else carries on.
func TestExampleIgnoresOtherMessages(t *testing.T) {
	h := host(t)
	for _, m := range []feature.Message{
		{Chat: feature.Chat{User: "u-bob"}, Text: "/example please", Addressed: true},
		{Chat: feature.Chat{User: "u-bob"}, Text: "an example", Addressed: true},
		{Chat: feature.Chat{GroupID: "g1"}, Text: "/example"}, // not addressed
	} {
		if a := h.Message(context.Background(), m); a != (feature.Action{}) {
			t.Errorf("%+v got %+v", m, a)
		}
	}
}

package feature_test

import (
	"context"
	"testing"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/feature/featuretest"
)

func TestParseCommand(t *testing.T) {
	for text, want := range map[string][2]string{
		" /Quote  NVDA AMD ": {"quote", "NVDA AMD"},
		"/help":              {"help", ""},
		"/x\nnext line":      {"x", "next line"},
	} {
		name, args, ok := feature.ParseCommand(text)
		if !ok || name != want[0] || args != want[1] {
			t.Errorf("%q: got %q %q %v", text, name, args, ok)
		}
	}
	for _, text := range []string{"hello", "/", " / x"} {
		if _, _, ok := feature.ParseCommand(text); ok {
			t.Errorf("%q parsed as a command", text)
		}
	}
}

func TestCommand(t *testing.T) {
	h := featuretest.New()
	err := feature.Command(h, "ping", func(_ context.Context, m feature.Message, args string) feature.Action {
		return feature.Action{Answer: "pong " + args}
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if a := h.Message(ctx, feature.Message{Text: "/PING now", Addressed: true}); a.Answer != "pong now" {
		t.Errorf("got %+v", a)
	}
	if a := h.Message(ctx, feature.Message{Text: "/ping"}); a != (feature.Action{}) {
		t.Errorf("an unaddressed message ran the command: %+v", a)
	}
	if a := h.Message(ctx, feature.Message{Text: "/pinger", Addressed: true}); a != (feature.Action{}) {
		t.Errorf("another name ran the command: %+v", a)
	}
}

// A name is checked when it is registered: taken names and bad spellings are refused.
func TestCommandNamesRefused(t *testing.T) {
	h := featuretest.New()
	nop := func(context.Context, feature.Message, string) feature.Action { return feature.Action{} }
	if err := feature.Command(h, "ping", nop); err != nil {
		t.Fatal(err)
	}
	if err := feature.Command(h, "ping", nop); err == nil {
		t.Error("a taken name was accepted")
	}
	for _, name := range []string{"", "Ping", "pi ng", "/ping"} {
		if err := feature.Command(h, name, nop); err == nil {
			t.Errorf("%q was accepted", name)
		}
	}
	if err := feature.Command(featuretest.New(), "ping", nop); err != nil {
		t.Errorf("a name taken on another host was refused: %v", err)
	}
}

func TestChatKey(t *testing.T) {
	if k := (feature.Chat{GroupID: "G"}).Key(); k != "g:G" {
		t.Error(k)
	}
	if k := (feature.Chat{User: "U"}).Key(); k != "u:U" {
		t.Error(k)
	}
}

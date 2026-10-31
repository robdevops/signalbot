package groupctx

import (
	"context"
	"strings"
	"testing"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/feature/featuretest"
)

func host(t *testing.T, setting string) *featuretest.Host {
	t.Helper()
	h := featuretest.New()
	h.Settings["GROUP_CONTEXT"] = setting
	if err := Register(h); err != nil {
		t.Fatal(err)
	}
	return h
}

func chatterFrom(name, text string, media ...feature.Media) feature.Message {
	return feature.Message{Chat: feature.Chat{GroupID: "g1"}, Sender: "u-" + strings.ToLower(name), Name: name, Text: text, Media: media}
}

// A group message that isn't for the bot is kept as context, with its
// sender's name, or "someone" without one.
func TestGroupChatterIsContext(t *testing.T) {
	h := host(t, "") // on by default
	ctx := context.Background()
	if a := h.Message(ctx, chatterFrom("Alice", "the moon is made of cheese")); a != (feature.Action{Context: "Alice: the moon is made of cheese"}) {
		t.Fatalf("got %+v", a)
	}
	if a := h.Message(ctx, chatterFrom("", "hello")); a.Context != "someone: hello" {
		t.Fatalf("no name: got %+v", a)
	}
	long := chatterFrom("Bob", strings.Repeat("é", 3000))
	if a := h.Message(ctx, long); len([]rune(a.Context)) != len("Bob: ")+maxChars+1 || !strings.HasSuffix(a.Context, "…") {
		t.Fatalf("not capped: %d runes", len([]rune(a.Context)))
	}
}

// Photos and stickers are noted, never fetched (the fake host has no media,
// so a fetch would fail); a quoted photo isn't the message's own. A message
// with nothing to keep, such as a group update, adds nothing.
func TestGroupChatterNotes(t *testing.T) {
	h := host(t, "true")
	ctx := context.Background()
	photo := feature.Media{ID: "a1", Type: "image/jpeg"}
	sticker := feature.Media{Pack: "p1", Sticker: 2}
	quoted := feature.Media{ID: "a2", Type: "image/png", Quoted: true}
	for _, c := range []struct {
		m    feature.Message
		want string
	}{
		{chatterFrom("Alice", "", photo), "Alice: [image]"},
		{chatterFrom("Alice", "look", photo, photo), "Alice: look [image]"},
		{chatterFrom("Bob", "", sticker), "Bob: [sticker]"},
		{chatterFrom("Bob", "ha", photo, sticker), "Bob: ha [image] [sticker]"},
		{chatterFrom("Bob", "", quoted), ""},
		{chatterFrom("Alice", ""), ""},
	} {
		if a := h.Message(ctx, c.m); a.Context != c.want {
			t.Errorf("%+v: got %q, want %q", c.m, a.Context, c.want)
		}
	}
}

// An addressed message is the core's to answer: Context doesn't count on
// it. (The core runs the hooks on no unaddressed DM.)
func TestAddressedIsNotContext(t *testing.T) {
	h := host(t, "")
	addressed := chatterFrom("Alice", "what do you think?")
	addressed.Addressed = true
	if a := h.Message(context.Background(), addressed); a != (feature.Action{}) {
		t.Errorf("got %+v", a)
	}
}

// With GROUP_CONTEXT off, chatter is dropped untouched.
func TestGroupContextOff(t *testing.T) {
	for _, v := range []string{"false", "off", "0"} {
		h := host(t, v)
		if a := h.Message(context.Background(), chatterFrom("Alice", "hello")); a != (feature.Action{}) {
			t.Errorf("GROUP_CONTEXT=%s: got %+v", v, a)
		}
	}
}

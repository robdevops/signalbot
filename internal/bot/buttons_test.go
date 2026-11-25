package bot

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/feature"
)

// answered is a press's Answer that hands what it showed to a channel.
func answered() (func(string), chan string) {
	got := make(chan string, 2)
	return func(text string) { got <- text }, got
}

func next(t *testing.T, got chan string) string {
	t.Helper()
	select {
	case s := <-got:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("the press wasn't answered")
		return ""
	}
}

// A press goes to the OnPress hooks in its chat's slot, the first that
// takes it answering; one none takes, or with no hooks at all, is
// answered with nothing, once.
func TestPress(t *testing.T) {
	var seen []feature.Press
	h := start(t, options{features: func(b *Bot) {
		b.OnPress(func(_ context.Context, p feature.Press) (string, bool) {
			seen = append(seen, p)
			return "not mine", false
		})
		b.OnPress(func(_ context.Context, p feature.Press) (string, bool) {
			return "7 days", p.Data == "rain 7d"
		})
	}})
	p := feature.Press{Chat: feature.Chat{User: aliceID}, By: feature.Person{ID: aliceID, Name: "Alice"}, Sent: 5001, Data: "rain 7d"}
	answer, got := answered()
	h.net.handle(Pressed{Press: p, Answer: answer})
	if s := next(t, got); s != "7 days" {
		t.Fatalf("answered %q", s)
	}
	p.Data = "other"
	h.net.handle(Pressed{Press: p, Answer: answer})
	if s := next(t, got); s != "" {
		t.Fatalf("answered %q", s)
	}
	h.settle()
	if len(seen) != 2 || seen[0].Sent != 5001 || seen[0].By.ID != aliceID || seen[0].Chat.User != aliceID {
		t.Fatalf("seen %+v", seen)
	}
	select {
	case s := <-got:
		t.Fatalf("answered twice: %q", s)
	default:
	}
	h.noSend()

	bare := start(t, options{})
	answer, got = answered()
	bare.net.handle(Pressed{Press: p, Answer: answer})
	if s := next(t, got); s != "" {
		t.Fatalf("with no hook, answered %q", s)
	}
}

// An edit goes through the chat's mailbox to the network's Edit, keeping
// nothing in history; OnSent gets what stands for the message, or none if
// the edit failed, and it isn't dropped either way.
func TestEditPost(t *testing.T) {
	h := start(t, options{})
	chat := feature.Chat{User: aliceID}
	type result struct {
		sent    []int64
		dropped bool
	}
	got := make(chan result, 2)
	onSent := func(sent []int64, dropped bool) { got <- result{sent, dropped} }
	mk := feature.Markup{Buttons: [][]feature.Button{{{Label: "7d", Data: "rain 7d"}}}}
	h.b.Post(feature.Post{Chat: chat, Label: "Eco", Text: "Rain, 7 days", Edit: 5001, Markup: mk, OnSent: onSent})
	s := h.nextSend()
	if s.edit != 5001 || s.text != "Rain, 7 days" || len(s.markup.Buttons) != 1 {
		t.Fatalf("sent %+v", s)
	}
	if r := <-got; !slices.Equal(r.sent, []int64{5001}) || r.dropped {
		t.Fatalf("OnSent %+v", r)
	}
	h.net.fail.Store(true)
	h.b.Post(feature.Post{Chat: chat, Text: "again", Edit: 5001, OnSent: onSent})
	if r := <-got; r.sent != nil || r.dropped {
		t.Fatalf("a failed edit's OnSent %+v", r)
	}
	h.net.fail.Store(false)
	// Nothing was kept: the model's next request has no post in it.
	r, _ := h.ask(dm("hi"))
	if c := contents(r); len(c) != 1 {
		t.Fatalf("history %q", c)
	}
}

// A post's keyboard, in a DM on a network that shows one, makes a message
// of exactly one of its labels its command, across a restart, until a post
// takes it away. Elsewhere a label is only text.
func TestKeyboard(t *testing.T) {
	dir := t.TempDir()
	features := func(b *Bot) {
		b.Command("rain", func(_ context.Context, _ feature.Message, args string) feature.Action {
			return feature.Action{Answer: "rain " + args}
		})
	}
	run := func(buttons bool) *harness {
		return start(t, options{features: features, buttons: buttons, cfg: func(c *config.Config) { c.StateDir = dir }})
	}
	keys := [][]feature.Key{{{Label: "🌧 Rain", Command: "/rain 7d"}, {Label: "Now", Command: "/rain"}}}
	alice := feature.Chat{User: aliceID}

	h := run(true)
	h.b.Post(feature.Post{Chat: alice, Text: "Pick one", Markup: feature.Markup{Keyboard: keys}})
	if s := h.nextSend(); len(s.markup.Keyboard) != 1 {
		t.Fatalf("sent %+v", s)
	}
	h.receive(dm("🌧 Rain"))
	if s := h.nextSend(); s.text != "rain 7d" {
		t.Fatalf("answered %q", s.text)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "keyboards.json")); err != nil || strings.Contains(string(data), aliceID) {
		t.Fatalf("saved %s, %v", data, err)
	}
	// In a group, a label is only text, for the model.
	h.receive(atBot("🌧 Rain"))
	if s := h.nextSend(); s.text != "ok" {
		t.Fatalf("in a group, answered %q", s.text)
	}
	h.b.Shutdown(time.Second)

	h = run(true) // after a restart
	h.receive(dm("Now"))
	if s := h.nextSend(); s.text != "rain " {
		t.Fatalf("after a restart, answered %q", s.text)
	}
	h.b.Post(feature.Post{Chat: alice, Text: "Bye", Markup: feature.Markup{NoKeyboard: true}})
	h.nextSend()
	h.receive(dm("Now"))
	if s := h.nextSend(); s.text != "ok" {
		t.Fatalf("with the keyboard taken away, answered %q", s.text)
	}
	h.b.Shutdown(time.Second)

	// A network that shows no keyboard: none is remembered.
	h = run(false)
	h.b.Post(feature.Post{Chat: alice, Text: "Pick one", Markup: feature.Markup{Keyboard: keys}})
	h.nextSend()
	h.receive(dm("Now"))
	if s := h.nextSend(); s.text != "ok" {
		t.Fatalf("on a network without keyboards, answered %q", s.text)
	}
}

// A command's answer carries its markup to the network, and its OnSent
// learns what it went out as.
func TestAnswerMarkup(t *testing.T) {
	mk := feature.Markup{Buttons: [][]feature.Button{{{Label: "🔄", Data: "again"}}}}
	sentAs := make(chan []int64, 1)
	h := start(t, options{buttons: true, features: func(b *Bot) {
		b.Command("rain", func(context.Context, feature.Message, string) feature.Action {
			return feature.Action{Answer: "rain", Markup: mk, OnSent: func(sent []int64, _ bool) { sentAs <- sent }}
		})
	}})
	h.receive(dm("/rain"))
	if s := h.nextSend(); s.text != "rain" || len(s.markup.Buttons) != 1 {
		t.Fatalf("sent %+v", s)
	}
	if s := <-sentAs; len(s) != 1 {
		t.Fatalf("OnSent %v", s)
	}
}

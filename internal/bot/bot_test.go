package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/feature"
)

// With no features, the prompt is the golden one, whatever the model can do.
func TestGoldenPromptNoFeatures(t *testing.T) {
	want, err := os.ReadFile("../../testdata/prompt_nofeatures.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, tools := range []bool{false, true} {
		h := start(t, options{tools: tools})
		r, _ := h.ask(atBot("how did AAPL do against the S&P 500 this year?"))
		if got := system(r); got != string(want) {
			t.Fatalf("tools %v: system prompt\n got  %q\n want %q", tools, got, want)
		}
		if _, ok := r["tools"]; ok {
			t.Fatalf("tools offered: %v", r["tools"])
		}
	}
}

// An addressed group message is answered: logged without content, shown
// with the sender's name, with the names it showed linked in the reply, and
// remembered.
func TestGroupMentionIsAnswered(t *testing.T) {
	h := start(t, options{})
	h.answer(text("hi @Alice, ask @Bob"))
	r, s := h.ask(atBot("hello there"))
	equal(t, "model got", contents(r), []string{"user: Alice: hello there"})
	// Only Alice was named to the model, so only she reaches the known map.
	if s.text != "hi @Alice, ask @Bob" || s.chat.GroupID != groupID || len(s.known) != 1 || s.known["alice"] != aliceID {
		t.Fatalf("sent %+v", s)
	}
	r, _ = h.ask(atBot("and again"))
	equal(t, "history", contents(r), []string{"user: Alice: hello there", "assistant: hi @Alice, ask @Bob", "user: Alice: and again"})
	if n := h.typing(); n != 2 {
		t.Fatalf("typing %d times", n)
	}
	logs := h.logs.String()
	if !strings.Contains(logs, "<6>received group chat\n") || !strings.Contains(logs, "<6>reply 0.") || strings.Contains(logs, "Alice") || strings.Contains(logs, "hello") || strings.Contains(logs, "again") {
		t.Fatalf("logs:\n%s", logs)
	}
}

func TestDMIsAnswered(t *testing.T) {
	h := start(t, options{})
	r, s := h.ask(dm("what is up"))
	equal(t, "model got", contents(r), []string{"user: what is up"})
	if s.chat != (feature.Chat{User: aliceID}) || len(s.known) != 0 {
		t.Fatalf("sent %+v", s)
	}
	if logs := h.logs.String(); !strings.Contains(logs, "<6>received dm\n") || strings.Contains(logs, "what is up") || strings.Contains(logs, "Alice") {
		t.Fatalf("logs:\n%s", logs)
	}
}

// A group change forgets the names the bot links in that group.
func TestGroupChangeForgetsNames(t *testing.T) {
	h := start(t, options{})
	h.ask(atBot("hello"))
	key := feature.Chat{GroupID: groupID}.Key()
	if len(h.b.chats.Names(key)) == 0 {
		t.Fatal("no names learned")
	}
	h.net.handle(Received{Chat: feature.Chat{GroupID: groupID}, GroupChange: true})
	if names := h.b.chats.Names(key); len(names) != 0 {
		t.Fatalf("names %v", names)
	}
	h.noSend()
}

// Group chatter, and a photo with no text, have nothing for the core to
// answer, and aren't logged.
func TestNothingToAnswer(t *testing.T) {
	h := start(t, options{})
	h.receive(chatter("the moon is made of cheese"))
	h.receive(with(dm(""), "photo1"))
	h.receive(dm("a question"))
	h.nextSend()
	h.noSend()
	if n := len(h.requests()); n != 1 {
		t.Fatalf("%d model requests", n)
	}
	if logs := h.logs.String(); strings.Contains(logs, "cheese") || strings.Count(logs, "received") != 2 {
		t.Fatalf("logs:\n%s", logs)
	}
}

func TestQuoteIsEscapedAndCapped(t *testing.T) {
	h := start(t, options{})
	for _, c := range []struct {
		text  string
		quote feature.Quote
		want  string
	}{
		{"is it?", feature.Quote{Author: "Bob", Text: "a \"line\"\nbreak"}, `[Replying to a message by "Bob": "a \"line\"\nbreak"]` + "\nis it?"},
		{"why?", feature.Quote{Bot: true, Text: strings.Repeat("x", 600)}, `[Replying to a message attributed to you: "` + strings.Repeat("x", 500) + `…"]` + "\nwhy?"},
		{"who?", feature.Quote{Text: "hm"}, `[Replying to a message by "someone": "hm"]` + "\nwho?"},
	} {
		m := dm(c.text)
		m.Quote = &c.quote
		r, _ := h.ask(m)
		if got := contents(r); got[len(got)-1] != "user: "+c.want {
			t.Fatalf("model got %q, want %q", got[len(got)-1], c.want)
		}
	}
}

// A failed request is apologised for, logged, and kept out of history.
func TestModelError(t *testing.T) {
	h := start(t, options{})
	h.answer(func() (int, string) {
		return 400, `{"error":{"message":"Input flagged","metadata":{"flagged_input":"secret words"}}}`
	})
	_, s := h.ask(dm("secret words"))
	if s.text != sorry {
		t.Fatalf("sent %+v", s)
	}
	if logs := h.logs.String(); !strings.Contains(logs, "<3>model error after ") || strings.Contains(logs, "Alice") || !strings.Contains(logs, "Input flagged") || strings.Contains(logs, "secret") {
		t.Fatalf("logs:\n%s", logs)
	}
	r, _ := h.ask(dm("try again"))
	equal(t, "history", contents(r), []string{"user: try again"})
}

// Messages in one chat are answered one at a time, in order, each seeing the
// ones before; at most 4 wait. Other chats go on meanwhile.
func TestMailbox(t *testing.T) {
	h := start(t, options{})
	h.holdModel()
	h.answer(text("one"))
	h.receive(dm("first"))
	h.eventually("the first request", func() bool { return len(h.requests()) == 1 })
	for _, s := range []string{"second", "3", "4", "5", "6"} {
		h.receive(dm(s))
	}
	h.eventually("the full mailbox", func() bool {
		return strings.Contains(h.logs.String(), "<4>the chat's mailbox is full, dropping message")
	})
	h.receive(atBot("meanwhile"))
	h.eventually("the other chat's request", func() bool { return len(h.requests()) == 2 })
	h.release()
	h.settle()
	rs := h.requests()
	if len(rs) != 3 {
		t.Fatalf("%d requests", len(rs))
	}
	// The four that fitted in the mailbox are answered together, after the
	// first, seeing it.
	equal(t, "the waiting ones together", contents(rs[2]), []string{"user: first", "assistant: one", "user: second\n3\n4\n5"})
}

// In a group, messages answered together keep their senders' names; a
// command among them is answered on its own, in order.
func TestWaitingMessagesInAGroup(t *testing.T) {
	h := start(t, options{})
	first := h.gate()
	h.answer(first.answer("one"))
	h.receive(atBot("compute xyz"))
	h.eventually("the first request", func() bool { return len(h.requests()) == 1 })
	h.receive(atBot("actually"))
	h.receive(atBot("forget it"))
	first.open()
	h.nextSend()
	h.nextSend()
	h.settle()
	rs := h.requests()
	if len(rs) != 2 {
		t.Fatalf("%d requests", len(rs))
	}
	got := contents(rs[1])
	if last := got[len(got)-1]; last != "user: Alice: actually\nAlice: forget it" {
		t.Fatalf("batched turn %q", last)
	}
}

// A chat's worker counts as work for config.Test, so a test can wait for
// the bot to be idle.
func TestWorkerIsBusy(t *testing.T) {
	h := start(t, options{})
	h.holdModel()
	h.receive(dm("first"))
	if h.b.cfg.Test.Idle() {
		t.Fatal("idle with a message to answer")
	}
	h.release()
	h.nextSend()
	h.eventually("the bot to be idle", h.b.cfg.Test.Idle)
}

func TestAtCapacity(t *testing.T) {
	h := start(t, options{cfg: func(c *config.Config) { c.MaxConcurrency = 1 }})
	h.holdModel()
	h.receive(dm("first"))
	h.eventually("the first request", func() bool { return len(h.requests()) == 1 })
	h.receive(atBot("elsewhere"))
	h.eventually("the warning", func() bool { return strings.Contains(h.logs.String(), "<4>at capacity (1), dropping message\n") })
	h.release()
	h.nextSend()
	h.noSend()
}

func TestRepeatIsDropped(t *testing.T) {
	h := start(t, options{})
	h.ask(dm("same"))
	h.receive(dm("same"))
	h.receive(dm("different"))
	h.nextSend()
	h.noSend()
	if n := len(h.requests()); n != 2 || !strings.Contains(h.logs.String(), "<7>ignored duplicate\n") {
		t.Fatalf("%d requests; logs:\n%s", n, h.logs)
	}
}

// Messages that arrive before the bot's own ID is known wait in their chat's
// mailbox; an addressed one among them is answered, not queued again behind
// the others.
func TestMessagesBeforeTheIDIsKnown(t *testing.T) {
	h := start(t, options{waitID: true})
	h.receive(atBot("are you there?"))
	for i := range 4 {
		h.receive(chatter(fmt.Sprint("chatter ", i)))
	}
	h.receive(feature.Message{Chat: feature.Chat{User: bobID}, Sender: bobID, Text: "hi"}) // a stranger's DM, in a second slot
	h.eventually("both chats to wait", func() bool { return len(h.b.slots) == 2 })
	close(h.net.id)
	if s := h.nextSend(); s.text != "ok" || s.chat.GroupID != groupID {
		t.Fatalf("sent %+v", s)
	}
	h.noSend()
	h.settle()
	if logs := h.logs.String(); strings.Contains(logs, "dropping") {
		t.Fatalf("logs:\n%s", logs)
	}
}

// History follows the disappearing-messages timer, and with it goes what
// was kept about the chat.
func TestDisappearingHistory(t *testing.T) {
	h := start(t, options{})
	m := atBot("short-lived")
	m.Expires = time.Second
	h.ask(m)
	key := "g:" + groupID
	if len(h.b.chats.History(key)) != 2 || len(h.b.chats.Names(key)) == 0 {
		t.Fatal("nothing kept")
	}
	h.eventually("history to expire", func() bool { return len(h.b.chats.History(key)) == 0 && len(h.b.chats.Names(key)) == 0 })
}

// Shutdown lets a reply in flight finish, then nothing new starts.
func TestShutdownDrains(t *testing.T) {
	h := start(t, options{})
	h.holdModel()
	h.receive(dm("first"))
	h.eventually("the request", func() bool { return len(h.requests()) == 1 })
	done := make(chan struct{})
	go func() { defer close(done); h.b.Shutdown(5 * time.Second) }()
	time.Sleep(20 * time.Millisecond)
	h.release()
	if s := h.nextSend(); s.text != "ok" {
		t.Fatalf("sent %+v", s)
	}
	<-done
	h.receive(dm("too late"))
	h.noSend()
	h.eventually("timers to stop", func() bool { _, err := h.b.At("late", time.Now(), nil); return err != nil })
}

// A message a hook acts on is answered on its own, in order, between the
// runs of waiting messages answered together.
func TestWaitingCommandOnItsOwn(t *testing.T) {
	h := start(t, options{features: func(b *Bot) {
		b.OnMessage(func(_ context.Context, m feature.Message) feature.Action {
			if m.Text == "/ping" {
				return feature.Action{Answer: "pong"}
			}
			return feature.Action{}
		})
	}})
	first := h.gate()
	h.answer(first.answer("one"))
	h.receive(dm("first"))
	h.eventually("the first request", func() bool { return len(h.requests()) == 1 })
	for _, s := range []string{"a", "/ping", "b"} {
		h.receive(dm(s))
	}
	first.open()
	var sent []string
	for range 4 {
		sent = append(sent, h.nextSend().text)
	}
	h.settle()
	h.noSend()
	rs := h.requests()
	if len(rs) != 3 || !slices.Equal(sent, []string{"one", "ok", "pong", "ok"}) {
		t.Fatalf("%d requests, sent %q", len(rs), sent)
	}
	equal(t, "a alone", contents(rs[1])[2:], []string{"user: a"})
	if got := contents(rs[2]); got[len(got)-1] != "user: b" {
		t.Fatalf("b's turn %q", got)
	}
}

// While a batch is being answered, 4 more messages can wait, and they are
// answered together after it, in one batch.
func TestFourWaitDuringABatch(t *testing.T) {
	h := start(t, options{})
	first, second := h.gate(), h.gate()
	h.answer(first.answer("one"), second.answer("two"))
	h.receive(dm("first"))
	h.eventually("the first request", func() bool { return len(h.requests()) == 1 })
	for _, s := range []string{"b", "c", "d", "e", "dropped"} {
		h.receive(dm(s))
	}
	h.eventually("the full mailbox", func() bool { return strings.Contains(h.logs.String(), "the chat's mailbox is full, dropping message") })
	first.open()
	h.eventually("the batch's request", func() bool { return len(h.requests()) == 2 })
	for _, s := range []string{"f", "g", "h", "i"} {
		h.receive(dm(s))
	}
	if n := strings.Count(h.logs.String(), "mailbox is full"); n != 1 {
		t.Fatalf("%d drops; logs:\n%s", n, h.logs)
	}
	second.open()
	for range 3 {
		h.nextSend()
	}
	h.settle()
	rs := h.requests()
	if len(rs) != 3 {
		t.Fatalf("%d requests", len(rs))
	}
	equal(t, "the first batch", contents(rs[1])[2:], []string{"user: b\nc\nd\ne"})
	if got := contents(rs[2]); got[len(got)-1] != "user: f\ng\nh\ni" {
		t.Fatalf("the second batch %q", got)
	}
}

// A panic while handling one waiting message loses only that message.
func TestPanicInABatch(t *testing.T) {
	h := start(t, options{features: func(b *Bot) {
		b.OnMessage(func(_ context.Context, m feature.Message) feature.Action {
			if m.Text == "boom" {
				panic("a feature bug")
			}
			return feature.Action{}
		})
	}})
	first := h.gate()
	h.answer(first.answer("one"))
	h.receive(dm("first"))
	h.eventually("the first request", func() bool { return len(h.requests()) == 1 })
	for _, s := range []string{"b", "boom", "d"} {
		h.receive(dm(s))
	}
	first.open()
	h.nextSend()
	h.nextSend()
	h.settle()
	rs := h.requests()
	if len(rs) != 2 || !strings.Contains(h.logs.String(), "internal error while handling a message") {
		t.Fatalf("%d requests; logs:\n%s", len(rs), h.logs)
	}
	if got := contents(rs[1]); got[len(got)-1] != "user: b\nd" {
		t.Fatalf("batch %q", got)
	}
}

// Once shutdown begins, nothing more of what was waiting starts: not the
// run already gathered, not the command being handled, and not even the
// hooks for the rest.
func TestShutdownStopsWaiting(t *testing.T) {
	var late atomic.Bool
	stopped := make(chan struct{})
	h := start(t, options{features: func(b *Bot) {
		b.OnMessage(func(_ context.Context, m feature.Message) feature.Action {
			late.Store(late.Load() || m.Text == "b")
			if m.Text == "/ping" { // shutdown begins while it is handled
				go func() { defer close(stopped); b.Shutdown(5 * time.Second) }()
				for !b.closing.Load() {
					time.Sleep(time.Millisecond)
				}
				return feature.Action{Answer: "pong"}
			}
			return feature.Action{}
		})
	}})
	first := h.gate()
	h.answer(first.answer("one"))
	h.receive(dm("first"))
	h.eventually("the first request", func() bool { return len(h.requests()) == 1 })
	for _, s := range []string{"a", "/ping", "b"} {
		h.receive(dm(s))
	}
	first.open()
	h.nextSend() // one
	<-stopped
	h.noSend()
	if n := len(h.requests()); n != 1 || late.Load() {
		t.Fatalf("%d requests; the hooks saw b: %v", n, late.Load())
	}
}

// Waiting messages whose photos together pass 4 are answered in two turns,
// so no turn carries more pictures than one message may.
func TestWaitingPhotosSplit(t *testing.T) {
	var media []int
	h := start(t, options{features: func(b *Bot) {
		b.OnTurn(func(_ context.Context, m feature.Message) feature.Additions {
			media = append(media, len(m.Media))
			return feature.Additions{}
		})
	}})
	first := h.gate()
	h.answer(first.answer("one"))
	h.receive(dm("first"))
	h.eventually("the first request", func() bool { return len(h.requests()) == 1 })
	h.receive(with(dm("these"), "p1", "p2", "p3"))
	h.receive(with(dm("and these"), "p4", "p5"))
	first.open()
	for range 3 {
		h.nextSend()
	}
	h.settle()
	if !slices.Equal(media, []int{0, 3, 2}) {
		t.Fatalf("pictures per turn %v", media)
	}
}

// A sender's name never offers a keyword-gated tool group, alone or in a
// batch: only what was typed counts.
func TestKeywordsIgnoreNames(t *testing.T) {
	h := start(t, options{tools: true, features: func(b *Bot) {
		b.Tools(feature.ToolGroup{Names: []string{"lookups"}, Listing: "lookups", Keywords: regexp.MustCompile(`(?i)\blookup\b`),
			Tools: []feature.Tool{{Name: "do_lookup", Schema: json.RawMessage(`{"type":"object"}`),
				Run: func(context.Context, string) (string, []feature.Picture, error) { return "", nil, nil }}}})
	}})
	first := h.gate()
	h.answer(first.answer("one"))
	lookup := func(text string) feature.Message { m := atBot(text); m.Name = "Lookup"; return m }
	h.receive(lookup("hello"))
	h.eventually("the first request", func() bool { return len(h.requests()) == 1 })
	h.receive(lookup("how are you"))
	fine := atBot("fine")
	fine.Sender, fine.Name = bobID, "Bob"
	h.receive(fine)
	first.open()
	h.nextSend()
	h.nextSend()
	h.settle()
	rs := h.requests()
	if len(rs) != 2 {
		t.Fatalf("%d requests", len(rs))
	}
	for i, r := range rs {
		if _, ok := r["tools"]; ok {
			t.Errorf("request %d offered tools: %v", i, r["tools"])
		}
	}
	if got := contents(rs[1]); got[len(got)-1] != "user: Lookup: how are you\nBob: fine" {
		t.Fatalf("batch %q", got)
	}
	h.ask(atBot("do a lookup"))
	if _, ok := h.requests()[2]["tools"]; !ok {
		t.Fatal("a typed keyword offered no tools")
	}
}

// Messages join a batch only while it stays within one message's pictures:
// 4 images and one sticker, images first. One with neither text nor
// pictures adds nothing.
func TestBatchPictures(t *testing.T) {
	photos := func(n int) feature.Message {
		m := feature.Message{Text: "look", Chat: feature.Chat{User: aliceID}}
		for i := range n {
			m.Media = append(m.Media, feature.Media{ID: fmt.Sprint(i)})
		}
		return m
	}
	sticker := feature.Message{Chat: feature.Chat{User: aliceID}, Media: []feature.Media{{ID: "p:1", Sticker: true}}}
	for _, c := range []struct {
		batch []feature.Message
		msg   feature.Message
		want  bool
	}{
		{nil, photos(4), true},
		{[]feature.Message{photos(2)}, photos(2), true},
		{[]feature.Message{photos(3)}, photos(2), false},
		{[]feature.Message{photos(4)}, sticker, true},
		{[]feature.Message{sticker}, sticker, false},
	} {
		if got := fits(c.batch, c.msg); got != c.want {
			t.Errorf("fits(%d messages, %d media) = %v", len(c.batch), len(c.msg.Media), got)
		}
	}
	m, _ := merge([]feature.Message{sticker, photos(2)})
	if len(m.Media) != 3 || !m.Media[2].Sticker || m.Text != "look" {
		t.Fatalf("merged %+v", m)
	}
	bare := feature.Message{Chat: feature.Chat{GroupID: groupID}, Name: "Alice"}
	if m, text := merge([]feature.Message{bare, bare}); m.Text != "" || text != "" || m.Media != nil {
		t.Fatalf("bare mentions merged to %+v, %q", m, text)
	}
	// The model sees each sender's name; the message's text, which tool
	// keywords match, is only what was typed.
	quote := &feature.Quote{Author: "Alice", Text: "a lookup of it"}
	m, text := merge([]feature.Message{bare, {Chat: bare.Chat, Name: "Bob", Text: "hi", Quote: quote}, {Chat: bare.Chat, Name: "Alice", Text: "and you?"}})
	if m.Text != "hi\nand you?" || text != "Bob: [Replying to a message by \"Alice\": \"a lookup of it\"]\nhi\nAlice: and you?" {
		t.Fatalf("merged %q, model sees %q", m.Text, text)
	}
}

// gate holds one model answer until it is opened.
type gate struct {
	c    chan struct{}
	once sync.Once
}

func (h *harness) gate() *gate {
	g := &gate{c: make(chan struct{})}
	h.t.Cleanup(g.open)
	return g
}

func (g *gate) open() { g.once.Do(func() { close(g.c) }) }

func (g *gate) answer(s string) func() (int, string) {
	return func() (int, string) { <-g.c; return 200, reply(s) }
}

// A reply that is one emoji is a reaction to the message answered, kept in
// history as "[reacted …]" and logged without the emoji; one with words is
// a message, and so is one to a message the bot made (no timestamp), which
// can't be reacted to.
func TestOneEmojiIsAReaction(t *testing.T) {
	h := start(t, options{})
	h.answer(text(" 🙏 "), text("👍 sure"), text("👍"), text("ok"))
	m := dm("thanks!")
	m.Timestamp = 1700
	if _, s := h.ask(m); s.text != "react 🙏 to "+aliceID+"@1700" || s.chat.User != aliceID {
		t.Fatalf("sent %+v", s)
	}
	reacted := func() int { return strings.Count(h.logs.String(), "<6>reacted instead of replying\n") }
	h.eventually("the reaction's log line", func() bool { return reacted() == 1 })
	m.Text, m.Timestamp = "great", 1701
	if _, s := h.ask(m); s.text != "👍 sure" {
		t.Fatalf("sent %q", s.text)
	}
	m.Text, m.Timestamp = "scheduled", 0
	if _, s := h.ask(m); s.text != "👍" {
		t.Fatalf("sent %q", s.text)
	}
	r, _ := h.ask(dm("what did you do?"))
	equal(t, "history", contents(r)[:2], []string{"user: thanks!", "assistant: [reacted 🙏]"})
	if reacted() != 1 || strings.Contains(h.logs.String(), "🙏") || strings.Contains(h.logs.String(), "👍") {
		t.Fatalf("log %q", h.logs.String())
	}
}

package bot

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/config"
)

// With no features, the prompt is the golden one pinned at e40dc5f, whatever
// the model can do.
func TestGoldenPromptNoFeatures(t *testing.T) {
	want, err := os.ReadFile("../../testdata/prompt_nofeatures.txt") // the one pinned at e40dc5f
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
// with the sender's name, linked mentions in the reply, and remembered.
func TestGroupMentionIsAnswered(t *testing.T) {
	h := start(t, options{})
	h.answer(text("hi @Alice, ask @Bob"))
	r, s := h.ask(atBot("hello there"))
	equal(t, "model got", contents(r), []string{"user: Alice: hello there"})
	if s["message"] != "hi @Alice, ask @Bob" || s["groupId"] != groupID {
		t.Fatalf("sent %v", s)
	}
	// Alice was named to the model; Bob was not, so he stays plain text.
	if m, _ := s["mention"].([]any); len(m) != 1 || m[0] != "3:6:"+aliceID {
		t.Fatalf("mentions %v", s["mention"])
	}
	r, _ = h.ask(atBot("and again"))
	equal(t, "history", contents(r), []string{"user: Alice: hello there", "assistant: hi @Alice, ask @Bob", "user: Alice: and again"})
	if typing := h.calledWith("sendTyping"); len(typing) < 2 {
		t.Fatalf("typing %v", typing)
	}
	logs := h.logs.String()
	if !strings.Contains(logs, "<6>received group chat\n") || !strings.Contains(logs, "<6>reply Alice ") || strings.Contains(logs, "hello") || strings.Contains(logs, "again") {
		t.Fatalf("logs:\n%s", logs)
	}
}

func TestAllowedDMIsAnsweredStrangerIsNot(t *testing.T) {
	h := start(t, options{})
	r, s := h.ask(dm("what is up"))
	equal(t, "model got", contents(r), []string{"user: what is up"})
	if s["recipient"].([]any)[0] != aliceID {
		t.Fatalf("sent %v", s)
	}
	h.receive(msg{from: bobID, name: "Bob", text: "let me in"})
	h.eventually("the stranger logged", func() bool {
		return strings.Contains(h.logs.String(), "<6>"+bobID+" ignored because not in SIGNAL_DM_USER_IDS\n")
	})
	h.noSend()
	if logs := h.logs.String(); !strings.Contains(logs, "<6>received dm\n") || strings.Contains(logs, "let me in") || strings.Contains(logs, "Bob(") {
		t.Fatalf("logs:\n%s", logs)
	}
}

// Group chatter, and a photo with no text, have nothing for the core to
// answer, and aren't logged.
func TestNothingToAnswer(t *testing.T) {
	h := start(t, options{})
	h.receive(chatter("the moon is made of cheese"))
	h.receive(msg{from: aliceID, name: "Alice", images: []string{"photo1"}})
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
		m    msg
		want string
	}{
		{msg{text: "is it?", quote: &quoted{author: bobID, text: "a \"line\"\nbreak"}}, `[Replying to a message by "Bob": "a \"line\"\nbreak"]` + "\nis it?"},
		{msg{text: "why?", quote: &quoted{author: botID, text: strings.Repeat("x", 600)}}, `[Replying to a message attributed to you: "` + strings.Repeat("x", 500) + `…"]` + "\nwhy?"},
		{msg{text: "who?", quote: &quoted{author: "33333333-3333-4333-8333-333333333333", text: "hm"}}, `[Replying to a message by "someone": "hm"]` + "\nwho?"},
	} {
		c.m.from, c.m.name = aliceID, "Alice"
		r, _ := h.ask(c.m)
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
	if s["message"] != sorry {
		t.Fatalf("sent %v", s)
	}
	if logs := h.logs.String(); !strings.Contains(logs, "<3>OpenRouter error for Alice after ") || !strings.Contains(logs, "Input flagged") || strings.Contains(logs, "secret") {
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
	if len(rs) != 6 {
		t.Fatalf("%d requests", len(rs))
	}
	equal(t, "the second sees the first", contents(rs[2]), []string{"user: first", "assistant: one", "user: second"})
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
	if n := len(h.requests()); n != 2 || !strings.Contains(h.logs.String(), "<7>ignored duplicate from Alice\n") {
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
	h.receive(msg{from: bobID, name: "Bob", text: "hi"}) // a DM, in a second slot: the frames above are all in
	h.eventually("both chats to wait", func() bool { return len(h.b.slots) == 2 })
	close(h.id)
	if s := h.nextSend(); s["message"] != "ok" || s["groupId"] != groupID {
		t.Fatalf("sent %v", s)
	}
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
	m.expires = 1
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
	if s := h.nextSend(); s["message"] != "ok" {
		t.Fatalf("sent %v", s)
	}
	<-done
	h.receive(dm("too late"))
	h.noSend()
	h.eventually("timers to stop", func() bool { _, err := h.b.At("late", time.Now(), nil); return err != nil })
}

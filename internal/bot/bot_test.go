package bot

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/llm"
)

// fakeTransport is a chat network in memory: Handle delivers to the bot,
// and what the bot sends is kept.
type fakeTransport struct {
	mu   sync.Mutex
	sent []chat.Outgoing
}

func (f *fakeTransport) Kind() string                                   { return "fake" }
func (f *fakeTransport) Run(ctx context.Context, _ func(chat.Incoming)) { <-ctx.Done() }
func (f *fakeTransport) Typing(chat.ChatID)                             {}
func (f *fakeTransport) Name(context.Context) string                    { return "Botty" }
func (f *fakeTransport) Describe() string                               { return "You are a helpful assistant in a test chat." }
func (f *fakeTransport) Forget(chat.ChatID)                             {}
func (f *fakeTransport) Fetch(context.Context, chat.ChatID, chat.Media) ([]byte, error) {
	return nil, nil
}
func (f *fakeTransport) Send(_ context.Context, m chat.Outgoing) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, m)
	return nil
}

func (f *fakeTransport) waitSent(t *testing.T, n int) []chat.Outgoing {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		f.mu.Lock()
		if len(f.sent) >= n {
			defer f.mu.Unlock()
			return append([]chat.Outgoing(nil), f.sent...)
		}
		f.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%d messages sent, want %d", len(f.sent), n)
	return nil
}

// echoAI answers with the last message it was sent, and keeps the request.
type echoAI struct {
	mu  sync.Mutex
	req llm.Request
}

func (a *echoAI) Complete(_ context.Context, r llm.Request) (llm.Completion, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.req = r
	return llm.Completion{Text: "you said: " + r.Messages[len(r.Messages)-1].Content}, nil
}

func newTestBot(ai llm.Completer) *Bot {
	return New(context.Background(), &Config{Model: "p/test-model", MaxConcurrency: 4, HistoryTurns: 2, Caps: feature.Caps{Tools: true}}, ai, &fakeTransport{})
}

// deliver hands m to b as an addressed message, as a transport would.
func deliver(b *Bot, m chat.Message) {
	b.Handle(chat.Incoming{Chat: m.Chat, Group: m.Group, Addressed: true, Load: func(context.Context) (chat.Message, bool) { return m, true }})
}

var alice = chat.Message{Chat: "fake:g:1", Group: true, Sender: chat.Person{ID: "u-alice", Name: "Alice"}, Text: "hi", Why: "mention"}

// With no features at all, the bot answers an addressed message with the
// model's reply, remembers the turn, and prompts with the transport's
// description and its own rules only.
func TestCoreWithNoFeatures(t *testing.T) {
	ai := &echoAI{}
	b := newTestBot(ai)
	deliver(b, alice)
	sent := b.transports["fake"].(*fakeTransport).waitSent(t, 1)
	if sent[0].Chat != "fake:g:1" || sent[0].Text != "you said: Alice: hi" {
		t.Fatalf("sent %+v", sent)
	}
	if h := b.History("fake:g:1"); len(h) != 2 || h[0].Content != "Alice: hi" || h[1].Content != "you said: Alice: hi" {
		t.Fatalf("history %+v", h)
	}
	ai.mu.Lock()
	defer ai.mu.Unlock()
	if ai.req.System != "You are a helpful assistant in a test chat."+basePrompt || len(ai.req.Tools) != 0 {
		t.Fatalf("system prompt %q, tools %v", ai.req.System, ai.req.Tools)
	}
	if b.toolsText() != "" {
		t.Fatalf("tools text %q", b.toolsText())
	}
}

// A command answers without the model, or forces a tool, which is then the
// only one offered.
func TestCommands(t *testing.T) {
	ai := &echoAI{}
	b := newTestBot(ai)
	group := feature.ToolGroup{Prompt: "things", Tools: []feature.Tool{{Name: "a"}, {Name: "b"}}}
	h := host{b}
	h.Tools(group)
	h.Command("hello", func(_ context.Context, _ feature.Turn, rest string) (feature.Command, bool) {
		if rest == "tool" {
			return feature.Command{Tool: "b"}, true
		}
		return feature.Command{Text: "hello yourself"}, true
	})
	if h.Command("hello", nil) == nil {
		t.Fatal("a taken command name was accepted")
	}
	m := alice
	m.Text = "/HELLO"
	deliver(b, m)
	tr := b.transports["fake"].(*fakeTransport)
	if sent := tr.waitSent(t, 1); sent[0].Text != "hello yourself" {
		t.Fatalf("sent %+v", sent)
	}
	m.Text = "/hello tool"
	deliver(b, m)
	tr.waitSent(t, 2)
	ai.mu.Lock()
	defer ai.mu.Unlock()
	if ai.req.Force != "b" || len(ai.req.Tools) != 1 || ai.req.Tools[0].Name != "b" || !strings.Contains(ai.req.System, "Your tools: things.") {
		t.Fatalf("forced turn: force %q, tools %v, prompt %q", ai.req.Force, ai.req.Tools, ai.req.System)
	}
}

// A model that can't call tools is offered none, and told of none.
func TestNoToolCalls(t *testing.T) {
	b := newTestBot(nil)
	b.cfg.Caps.Tools = false
	host{b}.Tools(feature.ToolGroup{Prompt: "things", Intro: "things", Tools: []feature.Tool{{Name: "a"}}})
	if p, offered := b.SystemPrompt(alice); strings.Contains(p, "Your tools") || len(offered) != 0 || b.toolsText() != "none" {
		t.Fatalf("prompt %q, offered %v, tools text %q", p, offered, b.toolsText())
	}
}

func TestStripSelfTags(t *testing.T) {
	for in, want := range map[string]string{
		"hi @bot there":           "hi there",
		"@bot hi":                 "hi",
		"@Bot, sure":              ", sure", // punctuation is left as is
		"thanks @bot!":            "thanks !",
		"a  b":                    "a  b", // untouched without a tag
		"hi @BOT":                 "hi",
		"ask @botty instead":      "ask @botty instead", // a different name
		"email bot@x.org":         "email bot@x.org",
		"line one\nline @bot two": "line one\nline two",
	} {
		if got := stripSelfTags(in, "bot"); got != want {
			t.Errorf("%q: got %q, want %q", in, got, want)
		}
	}
	if got := stripSelfTags("hi @bot", ""); got != "hi @bot" {
		t.Errorf("unknown self name must leave the reply alone: %q", got)
	}
}

func TestReplyIsCappedToAFewMessages(t *testing.T) {
	reply := replyText(strings.Repeat("spam ", 200000), "")
	if n := len([]rune(reply)); n > maxReplyRunes {
		t.Fatalf("one reply kept %d runes, want at most %d", n, maxReplyRunes)
	}
	if !strings.HasSuffix(reply, "[…truncated]") {
		t.Fatal("a truncated reply should say so")
	}
}

func TestDuplicateMessagesIgnoredWithinWindow(t *testing.T) {
	b := newTestBot(nil)
	req := &Request{Sender: "Alice(u)", Key: "g:1", Text: "hi"}
	if b.duplicate(req) {
		t.Fatal("first message flagged as duplicate")
	}
	if !b.duplicate(req) {
		t.Fatal("repeat within the window not flagged")
	}
	other := *req
	other.Text = "hello"
	if b.duplicate(&other) {
		t.Fatal("different text flagged as duplicate")
	}
	elsewhere := *req
	elsewhere.Key = "g:2"
	if b.duplicate(&elsewhere) {
		t.Fatal("same text in another chat flagged as duplicate")
	}
	b.mu.Lock()
	for k := range b.dedupe {
		b.dedupe[k] = time.Now().Add(-11 * time.Second)
	}
	b.mu.Unlock()
	if b.duplicate(req) {
		t.Fatal("message after the window flagged as duplicate")
	}
}

func TestShutdownWaitsForRepliesInFlight(t *testing.T) {
	b := newTestBot(nil)
	b.sem <- struct{}{} // a reply in flight holds a slot
	go func() { time.Sleep(50 * time.Millisecond); <-b.sem }()
	start := time.Now()
	b.Shutdown(2 * time.Second)
	if took := time.Since(start); took < 40*time.Millisecond || took > time.Second {
		t.Fatalf("Shutdown returned after %v", took)
	}

	b = newTestBot(nil)
	b.sem <- struct{}{} // a reply that never finishes
	start = time.Now()
	b.Shutdown(30 * time.Millisecond)
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Fatalf("Shutdown ignored its timeout: %v", took)
	}
}

func TestReplyLogLine(t *testing.T) {
	c := llm.Completion{InputTokens: 1203, CachedTokens: 128, OutputTokens: 312, Cost: 0.00215}
	got := replyLogLine("Alice", 3310*time.Millisecond, c)
	if want := "reply Alice 3.3s, tokens in/out: 1203/312 (10% cached), $0.002"; got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if got := replyLogLine("Alice", time.Second, llm.Completion{}); !strings.Contains(got, "(0% cached)") {
		t.Fatalf("no input tokens: %q", got)
	}
}

// The history records the charts a reply sent, so the model knows it did.
func TestSentImagesNote(t *testing.T) {
	res := llm.Completion{Images: []chat.Picture{{Data: []byte{1}, Caption: "Apple revenue — Q3"}, {Data: []byte{2}}}}
	if got := sentImages(res); got != "[sent the chart \"Apple revenue — Q3\"]\n[sent an image]\n" {
		t.Fatalf("got %q", got)
	}
}

func TestParseCommand(t *testing.T) {
	for text, want := range map[string][3]string{
		"/quote apple vs nvidia": {"quote", "apple vs nvidia", "true"},
		" /Quote ":               {"quote", "", "true"},
		"/":                      {"", "", "false"},
		"quote apple":            {"", "", "false"},
	} {
		n, r, ok := parseCommand(text)
		if n != want[0] || r != want[1] || (map[bool]string{true: "true", false: "false"})[ok] != want[2] {
			t.Errorf("%q: got %q %q %v", text, n, r, ok)
		}
	}
}

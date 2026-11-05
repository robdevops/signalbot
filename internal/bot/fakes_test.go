package bot

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

const (
	botID   = "00000000-0000-4000-8000-000000000000"
	aliceID = "11111111-1111-4111-8111-111111111111"
	bobID   = "22222222-2222-4222-8222-222222222222"
	groupID = "Z3JvdXA="
)

// harness is a bot on a fake network and a fake model API.
type harness struct {
	t    *testing.T
	b    *Bot
	net  *fakeNet
	logs *logx.Buffer

	mu      sync.Mutex
	asks    []map[string]any       // every model request body
	answers []func() (int, string) // queued model answers; then "ok"
	hold    chan struct{}          // while set, the model waits on it
	tools   bool                   // the model can call tools
	lists   int                    // model list requests
}

// fakeNet is the chat network, in memory. Its rules stand in for Signal's,
// which the signal package tests: a group's members are Alice and Bob,
// "@Botty " starts a mention of the bot in a delivered message, and an
// unaddressed DM is nothing to the core.
type fakeNet struct {
	id      chan struct{} // closed once the bot is connected
	running chan struct{} // closed once Run is receiving
	handle  func(Received)
	sent    chan sent // what the bot sends, in order
	mu      sync.Mutex
	typing  int      // Typing calls
	fetched []string // the media IDs fetched
}

type sent struct {
	chat   feature.Chat
	text   string
	images []feature.Picture
	known  map[string]string
}

func (n *fakeNet) Run(ctx context.Context, handle func(Received)) {
	n.handle = handle
	close(n.running)
	<-ctx.Done()
}

func (n *fakeNet) WaitID(ctx context.Context) string {
	select {
	case <-n.id:
		return botID
	case <-ctx.Done():
		return ""
	}
}

func (n *fakeNet) Send(chat feature.Chat, text string, images []feature.Picture, known map[string]string) {
	n.sent <- sent{chat, text, images, known}
}

// React is sent as "react <emoji> to <sender>@<timestamp>".
func (n *fakeNet) React(chat feature.Chat, msg feature.Message, emoji string) {
	n.sent <- sent{chat: chat, text: fmt.Sprintf("react %s to %s@%d", emoji, msg.Sender, msg.Timestamp)}
}

func (n *fakeNet) Typing(feature.Chat) func() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.typing++
	return func() {}
}

func (n *fakeNet) Delivered(ctx context.Context, msg feature.Message) (Received, bool) {
	if n.WaitID(ctx) == "" || msg.Chat.GroupID != "" && msg.Chat.GroupID != groupID {
		return Received{}, false
	}
	var r Received
	if msg.Chat.GroupID != "" {
		r.Members = []Member{{Name: "Alice", ID: aliceID}, {Name: "Bob", ID: bobID}}
	}
	msg.Name = cmp.Or(msg.Name, map[string]string{aliceID: "Alice", bobID: "Bob"}[msg.Sender])
	msg.Text, r.Mentioned = strings.CutPrefix(msg.Text, "@Botty ")
	r.Message = msg
	return r, true
}

func (n *fakeNet) BotName() string { return "Botty" }

// Prompt is Signal's, so the golden prompt holds with no features.
func (n *fakeNet) Prompt() string {
	return "This is a Signal chat. Never exceed 2000 characters, which is Signal's limit for one message."
}

func (n *fakeNet) Media(_ context.Context, msg feature.Message, i int) ([]byte, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.fetched = append(n.fetched, msg.Media[i].ID)
	return png, nil
}

func (n *fakeNet) Groups(context.Context, string) ([]feature.Group, error) { return nil, nil }

func (n *fakeNet) Redact(text string) string { return text }

// options for start.
type options struct {
	tools    bool
	otherAPI bool // the fake model API is not taken for OpenRouter's
	waitID   bool // the bot isn't connected until the test closes h.net.id
	features func(*Bot)
	cfg      func(*config.Config)
}

// start runs a bot on the fakes. Everything it started has stopped by the
// time the test ends.
func start(t *testing.T, o options) *harness {
	t.Helper()
	h := &harness{t: t, logs: &logx.Buffer{}, tools: o.tools,
		net: &fakeNet{id: make(chan struct{}), running: make(chan struct{}), sent: make(chan sent, 64)}}
	t.Cleanup(logx.SetOutput(h.logs, h.logs, true))
	if !o.waitID {
		close(h.net.id)
	}
	srv := httptest.NewServer(http.HandlerFunc(h.model))
	t.Cleanup(srv.Close)

	cfg := &config.Config{APIURL: srv.URL, APIKey: "k", Model: "test/test-model", Reasoning: "low", Intro: config.DefaultIntro, ToolLinger: 2,
		MaxConcurrency: 4, HistoryMax: 20, Test: &config.Test{}}
	if u, _ := url.Parse(srv.URL); !o.otherAPI {
		cfg.Test.OpenRouter = u.Hostname() // the fake is OpenRouter
	}
	if o.cfg != nil {
		o.cfg(cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.b = New(ctx, cfg, h.net)
	if o.features != nil {
		o.features(h.b)
	}
	done := make(chan struct{})
	go func() { defer close(done); h.b.Run() }()
	t.Cleanup(func() {
		h.release()
		if !h.b.closing.Load() {
			h.b.Shutdown(5 * time.Second)
		}
		cancel()
		<-done
	})
	<-h.net.running
	return h
}

// model is the fake model API, OpenRouter's unless otherAPI.
func (h *harness) model(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		h.mu.Lock()
		h.lists++
		h.mu.Unlock()
		params := `[]`
		if h.tools {
			params = `["tools"]`
		}
		fmt.Fprintf(w, `{"data":[{"id":"test/test-model","architecture":{"input_modalities":["text","image"]},"supported_parameters":%s}]}`, params)
		return
	}
	var body map[string]any
	data, _ := io.ReadAll(r.Body)
	json.Unmarshal(data, &body)
	h.mu.Lock()
	h.asks = append(h.asks, body)
	hold := h.hold
	answer := func() (int, string) { return 200, reply("ok") }
	if len(h.answers) > 0 {
		answer, h.answers = h.answers[0], h.answers[1:]
	}
	h.mu.Unlock()
	if hold != nil {
		<-hold
	}
	status, resp := answer()
	w.WriteHeader(status)
	io.WriteString(w, resp)
}

// reply is a model answer with text.
func reply(text string) string {
	b, _ := json.Marshal(text)
	return `{"choices":[{"message":{"content":` + string(b) + `}}],"usage":{"prompt_tokens":10,"completion_tokens":2,"cost":0.001}}`
}

// answer queues the model's next answers.
func (h *harness) answer(as ...func() (int, string)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.answers = append(h.answers, as...)
}

func text(s string) func() (int, string) { return func() (int, string) { return 200, reply(s) } }

// holdModel makes the model wait until release.
func (h *harness) holdModel() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hold = make(chan struct{})
}

func (h *harness) release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.hold != nil {
		close(h.hold)
		h.hold = nil
	}
}

// receive hands m to the bot as the network would: an addressed message
// to be loaded in its slot, where the names it shows are learned; other
// group messages as they are; an unaddressed DM not at all; and before the
// bot is connected, any message to wait in its slot.
func (h *harness) receive(m feature.Message) {
	load := func(ctx context.Context, learn func(name, id string)) (feature.Message, bool) {
		if h.net.WaitID(ctx) == "" {
			return feature.Message{}, false
		}
		if m.Addressed && m.Chat.GroupID != "" && m.Name != "" {
			learn(m.Name, m.Sender)
		}
		return m, m.Addressed || m.Chat.GroupID != ""
	}
	r := Received{Message: m}
	select {
	case <-h.net.id:
		switch {
		case m.Addressed:
			r = Received{Chat: m.Chat, Addressed: true, Load: load}
		case m.Chat.GroupID == "":
			return
		}
	default:
		r = Received{Chat: m.Chat, Load: load}
	}
	h.net.handle(r)
}

func dm(text string) feature.Message {
	return feature.Message{Chat: feature.Chat{User: aliceID}, Sender: aliceID, Name: "Alice", Text: text, Addressed: true}
}

func atBot(text string) feature.Message {
	return feature.Message{Chat: feature.Chat{GroupID: groupID}, Sender: aliceID, Name: "Alice", Text: text, Addressed: true}
}

func chatter(text string) feature.Message {
	return feature.Message{Chat: feature.Chat{GroupID: groupID}, Sender: bobID, Name: "Bob", Text: text}
}

// with adds images to m.
func with(m feature.Message, ids ...string) feature.Message {
	for _, id := range ids {
		m.Media = append(m.Media, feature.Media{ID: id})
	}
	return m
}

// nextSend is the next message the bot sends.
func (h *harness) nextSend() sent {
	h.t.Helper()
	select {
	case s := <-h.net.sent:
		return s
	case <-time.After(5 * time.Second):
		h.t.Fatalf("nothing sent; logs:\n%s", h.logs)
		return sent{}
	}
}

// noSend checks that the bot sends nothing more for a moment.
func (h *harness) noSend() {
	h.t.Helper()
	select {
	case s := <-h.net.sent:
		h.t.Fatalf("sent %+v", s)
	case <-time.After(100 * time.Millisecond):
	}
}

func (h *harness) typing() int {
	h.net.mu.Lock()
	defer h.net.mu.Unlock()
	return h.net.typing
}

// ask receives m and returns the model request and the reply sent.
func (h *harness) ask(m feature.Message) (map[string]any, sent) {
	h.t.Helper()
	n := len(h.requests())
	h.receive(m)
	s := h.nextSend()
	rs := h.requests()
	if len(rs) != n+1 {
		h.t.Fatalf("%d model requests, want %d", len(rs), n+1)
	}
	return rs[n], s
}

func (h *harness) requests() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]map[string]any(nil), h.asks...)
}

func (h *harness) eventually(what string, ok func() bool) {
	h.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s; logs:\n%s", what, h.logs)
		}
	}
}

// settle waits until no work is in flight.
func (h *harness) settle() {
	h.t.Helper()
	h.eventually("the bot to finish", func() bool { return len(h.b.slots) == 0 })
}

// system and history read a model request.
func system(r map[string]any) string { return messages(r)[0]["content"].(string) }

func messages(r map[string]any) []map[string]any {
	var out []map[string]any
	for _, m := range r["messages"].([]any) {
		out = append(out, m.(map[string]any))
	}
	return out
}

// contents is each message's text after the system prompt, as "role: text".
func contents(r map[string]any) []string {
	var out []string
	for _, m := range messages(r)[1:] {
		c, ok := m["content"].(string)
		if !ok { // content parts: the text first
			c = m["content"].([]any)[0].(map[string]any)["text"].(string)
		}
		out = append(out, m["role"].(string)+": "+c)
	}
	return out
}

func toolNames(r map[string]any) []string {
	var out []string
	ts, _ := r["tools"].([]any)
	for _, t := range ts {
		if f, ok := t.(map[string]any)["function"].(map[string]any); ok {
			out = append(out, fmt.Sprint(f["name"]))
		} else { // a server tool is its type, e.g. "openrouter:served"
			out = append(out, fmt.Sprint(t.(map[string]any)["type"]))
		}
	}
	return out
}

func equal(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%s:\n got  %q\n want %q", what, got, want)
	}
}

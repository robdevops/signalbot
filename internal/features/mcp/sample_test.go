package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	ext "github.com/robdevops/signalbot/addon"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/feature/featuretest"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

// --- in memory ------------------------------------------------------------------

// sampler is an addon allowed everywhere, with the model permission m, on a
// host whose model answers what said says, and records what it was sent.
func sampler(m *modelConfig) (*addon, *featuretest.Host, *[]map[string]any) {
	cfg := everything
	cfg.Model = m
	a, _ := unitAddon(cfg)
	h := featuretest.New()
	h.Bot.Model = "x/test-model"
	var mu sync.Mutex
	var bodies []map[string]any
	h.API = func(_ context.Context, path string, body map[string]any) (json.RawMessage, error) {
		if path != "/chat/completions" {
			return nil, errors.New("wrong path")
		}
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		return json.RawMessage(`{"model":"x/test-model-0611","choices":[{"message":{"role":"assistant","content":"Canberra"},"finish_reason":"length"}],` +
			`"usage":{"completion_tokens":3,"cost":0.0012}}`), nil
	}
	a.host = h
	return a, h, &bodies
}

// ask is a sampling request's params for chat, saying text.
func ask(chat, text string, maxTokens int) json.RawMessage {
	t := 0.5
	b, _ := json.Marshal(ext.SampleRequest{
		Messages:     []ext.SampleMessage{{Role: "user", Content: ext.Content{Type: "text", Text: text}}},
		SystemPrompt: "You write quiz questions.",
		MaxTokens:    maxTokens,
		Temperature:  &t,
		Meta:         ext.SampleMeta{Chat: chat},
	})
	return b
}

// A request is run on the bot's model, charged to the chat it names, with
// the system prompt first and maxTokens capped; the answer is MCP's; the
// log has no prompt or answer.
func TestSample(t *testing.T) {
	var log logx.Buffer
	t.Cleanup(logx.SetOutput(&log, &log, true))
	a, h, bodies := sampler(&modelConfig{PerHour: 30, MaxTokens: 100})
	a.lease(inA)
	res, rerr := a.sample(context.Background(), ask(a.chatID(inA), "Alice asks for the capital of Australia", 500))
	if rerr != nil {
		t.Fatal(rerr)
	}
	want := ext.SampleResult{Role: "assistant", Content: ext.Content{Type: "text", Text: "Canberra"}, Model: "x/test-model-0611", StopReason: "maxTokens"}
	if res != want {
		t.Fatalf("result %+v", res)
	}
	if c := h.Charged(); len(c) != 1 || c[0] != inA {
		t.Fatalf("charged %+v", c)
	}
	got, _ := json.Marshal((*bodies)[0])
	if w := `{"max_tokens":100,"messages":[{"content":"You write quiz questions.","role":"system"},` +
		`{"content":"Alice asks for the capital of Australia","role":"user"}],"temperature":0.5}`; string(got) != w {
		t.Fatalf("body %s", got)
	}
	l := log.String()
	if !strings.Contains(l, "<6>MCP server game: a model request: ok in ") || !strings.Contains(l, ", 3 tokens, $0.0012\n") {
		t.Fatalf("log %q", l)
	}
	for _, s := range []string{"Alice", "Canberra", "quiz"} {
		if strings.Contains(l, s) {
			t.Fatalf("%q reached the log: %q", s, l)
		}
	}
}

// A request is refused without the permission or a model, without a chat
// the addon may post in now, past the hourly rate, or too big or not text.
func TestSampleRefused(t *testing.T) {
	var log logx.Buffer
	t.Cleanup(logx.SetOutput(&log, &log, true))
	refused := func(a *addon, params json.RawMessage, code int) {
		t.Helper()
		if _, rerr := a.sample(context.Background(), params); rerr == nil || rerr.Code != code {
			t.Fatalf("got %v, want code %d", rerr, code)
		}
	}
	a, h, bodies := sampler(nil)
	a.lease(inA)
	refused(a, ask(a.chatID(inA), "hi", 10), -32601)

	a, h, bodies = sampler(&modelConfig{PerHour: 2, MaxTokens: 100})
	a.lease(inA)
	chat := a.chatID(inA)
	h.Bot.Model = ""
	refused(a, ask(chat, "hi", 10), -32601)
	h.Bot.Model = "x/test-model"

	refused(a, ask("", "hi", 10), -32602)
	refused(a, ask(a.chatID(inB), "hi", 10), -32602) // no lease
	refused(a, json.RawMessage(`{"messages":"hi"}`), -32602)
	refused(a, ask(chat, strings.Repeat("x", maxSampleText+1), 10), -32602)
	refused(a, ask(chat, "hi", 0), -32602)
	var req ext.SampleRequest
	json.Unmarshal(ask(chat, "hi", 10), &req)
	for range maxSampleMessages {
		req.Messages = append(req.Messages, req.Messages[0])
	}
	many, _ := json.Marshal(req)
	refused(a, many, -32602)
	req.Messages = []ext.SampleMessage{{Role: "user", Content: ext.Content{Type: "image", Data: tinyPNG(), MimeType: "image/png"}}}
	image, _ := json.Marshal(req)
	refused(a, image, -32602)
	req.Messages = []ext.SampleMessage{{Role: "system", Content: ext.Content{Type: "text", Text: "hi"}}}
	system, _ := json.Marshal(req)
	refused(a, system, -32602)
	if len(*bodies) != 0 || len(h.Charged()) != 0 {
		t.Fatalf("the model was asked: %v", *bodies)
	}

	for range 2 {
		if _, rerr := a.sample(context.Background(), ask(chat, "hi", 10)); rerr != nil {
			t.Fatal(rerr)
		}
	}
	refused(a, ask(chat, "hi", 10), -32000) // the hour's 2 are used

	// A chat the addon is no longer allowed in.
	a, _, _ = sampler(&modelConfig{PerHour: 2, MaxTokens: 100})
	a.lease(inA)
	a.cfg.Chats = []string{groupB}
	refused(a, ask(a.chatID(inA), "hi", 10), -32602)

	// The model failing, as when the chat is over its daily budget.
	a, h, _ = sampler(&modelConfig{PerHour: 2, MaxTokens: 100})
	a.lease(inA)
	h.API = func(context.Context, string, map[string]any) (json.RawMessage, error) {
		return nil, errors.New("the chat has reached its daily budget")
	}
	refused(a, ask(a.chatID(inA), "hi", 10), -32603)
	if strings.Contains(log.String(), "hi") {
		t.Fatalf("the prompt reached the log: %q", log.String())
	}
}

// --- through the addon package, against the bot's side ---------------------

// TestSampleAddonProcess is not a real test: run as a subprocess with the
// argument "sample", it is an addon built from the addon package that asks
// the bot's model. /ask asks it what its args say, in the command's chat
// ("cancel" gives up after 300ms; "crash" exits mid-request; "nochat" names
// no chat); /caps says what the bot offered; /round starts a reply watch.
func TestSampleAddonProcess(t *testing.T) {
	if flag.Arg(0) != "sample" {
		return
	}
	var s *ext.Server
	args := json.RawMessage(`{"type":"object","properties":{"args":{"type":"string"}}}`)
	s = ext.New(ext.Config{Name: "quiz", Title: "Quiz", Version: "1", Tools: []ext.Tool{
		{
			Name: "ask", Visibility: []string{"bot"}, Schema: args,
			Call: func(ctx context.Context, c *ext.Context, raw json.RawMessage) (ext.Result, error) {
				var a struct{ Args string }
				if err := ext.DecodeArgs(raw, &a); err != nil {
					return ext.Result{}, err
				}
				chat := c.Chat
				switch a.Args {
				case "nochat":
					chat = ""
				case "cancel":
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 300*time.Millisecond)
					defer cancel()
				case "crash":
					time.AfterFunc(300*time.Millisecond, func() { os.Exit(3) })
				}
				r, err := s.Sample(ctx, chat, ext.SampleRequest{
					Messages:  []ext.SampleMessage{{Role: "user", Content: ext.Content{Type: "text", Text: a.Args}}},
					MaxTokens: 50,
				})
				var be *ext.BotError
				switch {
				case errors.As(err, &be):
					return ext.Text(fmt.Sprintf("refused %d", be.Code)), nil
				case errors.Is(err, context.DeadlineExceeded):
					return ext.Text("gave up"), nil
				case err != nil:
					return ext.Text("failed"), nil
				}
				return ext.Text(r.Role + ": " + r.Content.Text + " (" + r.Model + ", " + r.StopReason + ")"), nil
			},
		},
		{
			Name: "caps", Visibility: []string{"bot"},
			Call: func(context.Context, *ext.Context, json.RawMessage) (ext.Result, error) {
				return ext.Text(fmt.Sprint(s.CanSample(), s.WatchReplies())), nil
			},
		},
		{
			Name: "round", Visibility: []string{"bot"},
			Call: func(_ context.Context, c *ext.Context, _ json.RawMessage) (ext.Result, error) {
				if err := s.Queue(ext.Op{Op: "post", Chat: c.Chat, Text: "Question 2"}); err != nil {
					return ext.Result{}, err
				}
				r := ext.Text("Question 1")
				r.Meta = &ext.ResultMeta{Watch: &ext.Watch{ID: "q", Replies: true, Until: time.Now().Add(time.Hour).Format(time.RFC3339), Tool: "answer"}}
				return r, nil
			},
		},
		{
			Name: "answer", Visibility: []string{"bot"}, Schema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
			Call: func(context.Context, *ext.Context, json.RawMessage) (ext.Result, error) {
				return ext.Result{Content: []ext.Content{{Type: "text", Text: "noted"}}, Meta: &ext.ResultMeta{React: "✅"}}, nil
			},
		},
	}})
	if err := s.Run(context.Background(), os.Stdin, os.Stdout); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// quizConfig is MCP_CONFIG for the package addon "quiz" in group A, with
// model as its addon block's "model" setting ("" for none).
func quizConfig(model string) string {
	block := `{"chats":["` + groupA + `"],"post":true,"react":true,"watch":true`
	if model != "" {
		block += `,"model":` + model
	}
	entry := strings.Replace(addonServer("sample", block+`}`, `,"chat_commands":{"ask":{"tool":"ask","direct":true},"caps":{"tool":"caps","direct":true},`+
		`"round":{"tool":"round","direct":true}}`), "TestAddonProcess", "TestSampleAddonProcess", 1)
	return `{"mcpServers":{"quiz":` + entry + `}}`
}

// modelAPI is a model API for the bot's side: it answers each prompt by
// echoing it, except "slow", which waits for release, and "cancel" and
// "crash", which answer after the addon has given up or exited, saying on
// ended whether their ctx had ended by then.
type modelAPI struct {
	called  chan string
	release chan struct{}
	ended   chan string
}

func newModelAPI() *modelAPI {
	return &modelAPI{called: make(chan string, 16), release: make(chan struct{}), ended: make(chan string, 16)}
}

func (m *modelAPI) call(ctx context.Context, _ string, body map[string]any) (json.RawMessage, error) {
	msgs := body["messages"].([]map[string]string)
	prompt := msgs[len(msgs)-1]["content"]
	m.called <- prompt
	switch prompt {
	case "slow":
		<-m.release
	case "cancel", "crash":
		time.Sleep(time.Second)
		m.ended <- fmt.Sprint(prompt, " ", ctx.Err())
	}
	b, _ := json.Marshal(prompt + "!")
	return json.RawMessage(`{"model":"x/test-model","choices":[{"message":{"content":` + string(b) + `},"finish_reason":"stop"}]}`), nil
}

// An addon allowed the model is offered sampling and reply watches, and
// its requests, with string IDs, are answered, one at a time; one it
// cancels, or that it exits during, still has its model call run to the
// end and charged to the chat.
func TestPackageAddonSample(t *testing.T) {
	defer func(d time.Duration) { restartDelay = d }(restartDelay)
	restartDelay = 10 * time.Millisecond
	api := newModelAPI()
	h, log := startAddon(t, quizConfig(`{}`), func(h *featuretest.Host) { h.API = api.call })
	ctx := context.Background()
	if got := h.Message(ctx, in(inA, "/caps")); got.Answer != "true true" {
		t.Fatalf("caps: %+v", got)
	}
	if got := h.Message(ctx, in(inA, "/ask hello")); got.Answer != "assistant: hello! (x/test-model, endTurn)" {
		t.Fatalf("ask: %+v", got)
	}
	if c := h.Charged(); len(c) != 1 || c[0] != inA {
		t.Fatalf("charged %+v", c)
	}
	<-api.called
	if got := h.Message(ctx, in(inA, "/ask nochat")); got.Answer != "refused -32602" {
		t.Fatalf("no chat: %+v", got)
	}

	// A second request while one is in flight is refused at once.
	slow := make(chan feature.Action)
	go func() { slow <- h.Message(ctx, in(inA, "/ask slow")) }()
	if p := <-api.called; p != "slow" {
		t.Fatalf("called with %q", p)
	}
	if got := h.Message(ctx, in(inA, "/ask hello")); got.Answer != "refused -32000" {
		t.Fatalf("a second in flight: %+v", got)
	}
	close(api.release)
	if got := <-slow; got.Answer != "assistant: slow! (x/test-model, endTurn)" {
		t.Fatalf("slow: %+v", got)
	}

	// The addon gives up: the bot's call still runs, and is charged.
	if got := h.Message(ctx, in(inA, "/ask cancel")); got.Answer != "gave up" {
		t.Fatalf("cancel: %+v", got)
	}
	select {
	case p := <-api.ended:
		if p != "cancel <nil>" {
			t.Fatalf("ended %q", p)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the model call didn't end")
	}
	<-api.called
	// The cancelled request is served until its call returns, just after.
	eventually(t, "the cancelled request's end", func() bool {
		return h.Message(ctx, in(inA, "/ask hello")).Answer == "assistant: hello! (x/test-model, endTurn)"
	})
	<-api.called

	// The addon exits mid-request: the call still runs, and the addon,
	// started again, is answered.
	h.Message(ctx, in(inA, "/ask crash"))
	select {
	case p := <-api.ended:
		if p != "crash <nil>" {
			t.Fatalf("ended %q", p)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the model call didn't end when the addon exited")
	}
	<-api.called
	eventually(t, "the restart", func() bool {
		return h.Message(ctx, in(inA, "/ask again")).Answer == "assistant: again! (x/test-model, endTurn)"
	})
	eventually(t, "the charges", func() bool {
		n := 0
		for _, c := range h.Charged() {
			if c == inA {
				n++
			}
		}
		return n >= 6 // hello, slow, cancel, hello, crash, again
	})
	for _, s := range []string{"hello", "slow!", "again"} {
		if strings.Contains(log.String(), s) {
			t.Fatalf("%q reached the log: %q", s, log.String())
		}
	}
}

// Without the permission, or with no model, sampling isn't offered, and a
// request is refused as unsupported.
func TestPackageAddonNoSample(t *testing.T) {
	for name, before := range map[string]func(*featuretest.Host){
		"no permission": func(*featuretest.Host) {},
		"no model":      func(h *featuretest.Host) { h.Bot.Model, h.Bot.CallsTools = "", false },
	} {
		t.Run(name, func(t *testing.T) {
			model := `{}`
			if name == "no permission" {
				model = ""
			}
			h, _ := startAddon(t, quizConfig(model), before, func(h *featuretest.Host) {
				h.API = func(context.Context, string, map[string]any) (json.RawMessage, error) {
					t.Error("the model was called")
					return nil, errors.New("no")
				}
			})
			if got := h.Message(context.Background(), in(inA, "/caps")); got.Answer != "false true" {
				t.Fatalf("caps: %+v", got)
			}
		})
	}
}

// A request that comes before the addon is ready waits for it, and one to
// a gate opened with no addon is refused, not called unsupported.
func TestSampleGate(t *testing.T) {
	a, _, _ := sampler(&modelConfig{PerHour: 30, MaxTokens: 100})
	a.lease(inA)
	g := &gate{ready: make(chan struct{})}
	got := make(chan *rpcError, 1)
	go func() {
		_, rerr := g.sample(context.Background(), ask(a.chatID(inA), "hi", 10))
		got <- rerr
	}()
	select {
	case rerr := <-got:
		t.Fatalf("answered before the addon was ready: %+v", rerr)
	case <-time.After(50 * time.Millisecond):
	}
	g.open(a)
	g.open(nil) // only the first counts
	if rerr := <-got; rerr != nil {
		t.Fatal(rerr)
	}

	g = &gate{ready: make(chan struct{})}
	g.open(nil)
	if _, rerr := g.sample(context.Background(), ask(a.chatID(inA), "hi", 10)); rerr == nil || rerr.Code != -32000 {
		t.Fatalf("no addon: %+v", rerr)
	}
	g = &gate{ready: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, rerr := g.sample(ctx, ask(a.chatID(inA), "hi", 10)); rerr == nil || rerr.Code != -32000 {
		t.Fatalf("cancelled while waiting: %+v", rerr)
	}
	var none *gate
	none.open(a) // does nothing
}

// A request the addon cancels still has its model call run, and charged
// to the chat, and gets no answer.
func TestSampleCancelledCharges(t *testing.T) {
	a, h, _ := sampler(&modelConfig{PerHour: 30, MaxTokens: 100})
	a.lease(inA)
	ctx, cancel := context.WithCancel(context.Background())
	api := h.API
	var callErr error
	h.API = func(c context.Context, path string, body map[string]any) (json.RawMessage, error) {
		cancel()
		callErr = c.Err()
		return api(c, path, body)
	}
	if _, rerr := a.sample(ctx, ask(a.chatID(inA), "hi", 10)); rerr == nil {
		t.Fatal("a cancelled request was answered")
	}
	if callErr != nil {
		t.Fatalf("the model call was cancelled: %v", callErr)
	}
	if c := h.Charged(); len(c) != 1 || c[0] != inA {
		t.Fatalf("charged %+v", c)
	}
}

// A request with a string ID gets an answer with that ID, and a cancel
// naming it cancels it; requests other than sampling and ping are still
// refused.
func TestServeRequests(t *testing.T) {
	r, w, _ := os.Pipe()
	defer r.Close()
	c := &stdioConn{name: "quiz", stdin: w, life: context.Background(), done: make(chan struct{})}
	started, ended := make(chan json.RawMessage, 1), make(chan error, 1)
	c.serveRequests(func(ctx context.Context, params json.RawMessage) (any, *rpcError) {
		started <- params
		if strings.Contains(string(params), "wait") {
			<-ctx.Done()
			ended <- ctx.Err()
			return nil, &rpcError{Code: -32603, Message: "cancelled"}
		}
		return map[string]string{"role": "assistant"}, nil
	})
	lines := make(chan string, 16)
	go func() {
		buf := make([]byte, 1<<16)
		var acc string
		for {
			n, err := r.Read(buf)
			acc += string(buf[:n])
			for {
				i := strings.IndexByte(acc, '\n')
				if i < 0 {
					break
				}
				lines <- acc[:i]
				acc = acc[i+1:]
			}
			if err != nil {
				return
			}
		}
	}()
	next := func() string {
		select {
		case l := <-lines:
			return l
		case <-time.After(5 * time.Second):
			t.Fatal("no line")
			return ""
		}
	}
	c.dispatch([]byte(`{"jsonrpc":"2.0","id":"q.1","method":"sampling/createMessage","params":{"x":1}}`))
	<-started
	if got := next(); got != `{"jsonrpc":"2.0","id":"q.1","result":{"role":"assistant"}}` {
		t.Fatalf("answer %s", got)
	}
	c.dispatch([]byte(`{"jsonrpc":"2.0","id":"q.2","method":"sampling/createMessage","params":{"wait":1}}`))
	<-started
	c.dispatch([]byte(`{"jsonrpc":"2.0","id":7,"method":"sampling/createMessage","params":{}}`))
	if got := next(); got != `{"jsonrpc":"2.0","id":7,"error":{"code":-32000,"message":"busy: one request at a time"}}` {
		t.Fatalf("second in flight: %s", got)
	}
	c.dispatch([]byte(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"q.2"}}`))
	if err := <-ended; !errors.Is(err, context.Canceled) {
		t.Fatalf("ended with %v", err)
	}
	c.dispatch([]byte(`{"jsonrpc":"2.0","id":"q.3","method":"roots/list"}`))
	if got := next(); got != `{"jsonrpc":"2.0","id":"q.3","error":{"code":-32601,"message":"method not supported"}}` {
		t.Fatalf("roots/list: %s (a cancelled request was answered?)", got)
	}
	c.dispatch([]byte(`{"jsonrpc":"2.0","id":"p","method":"ping"}`))
	if got := next(); got != `{"jsonrpc":"2.0","id":"p","result":{}}` {
		t.Fatalf("ping: %s", got)
	}
	eventually(t, "the cancelled request to end", func() bool { c.mu.Lock(); defer c.mu.Unlock(); return c.serving == nil })
	c.dispatch([]byte(`{"jsonrpc":"2.0","id":"q.4","method":"sampling/createMessage","params":{}}`))
	<-started
	if got := next(); got != `{"jsonrpc":"2.0","id":"q.4","result":{"role":"assistant"}}` {
		t.Fatalf("after a cancel: %s", got)
	}
	w.Close()
}

// --- reply watches ------------------------------------------------------------

// A reply watch takes only addressed replies to the addon's messages sent
// while it lives, and their edits; with a pattern, only those it matches.
// It knows the last maxSent, keeps them through a restart, and ends.
func TestReplyWatch(t *testing.T) {
	var log logx.Buffer
	t.Cleanup(logx.SetOutput(&log, &log, true))
	a, now := unitAddon(everything)
	cmd := said(inA, "alice-id", 1000, "/quiz")
	replies := func(id, pattern string) ext.Watch {
		return ext.Watch{ID: id, Pattern: pattern, Replies: true, Until: now.Add(time.Minute).Format(time.RFC3339), Tool: "guess"}
	}
	if a.watch(cmd, ext.Watch{ID: "x", Until: now.Add(time.Minute).Format(time.RFC3339), Tool: "guess"}) != nil {
		t.Fatal("a watch with no pattern and no replies was taken")
	}
	if a.watch(cmd, replies("q", "")) == nil {
		t.Fatalf("refused: %s", log.String())
	}
	a.replied(inA, []int64{5000})
	a.replied(inB, []int64{6000}) // no reply watch there

	answer := replyTo(said(inA, "bob-id", 1001, "Paris"), 5000)
	for _, m := range []feature.Message{answer, edit(answer, 1002, "Lyon"), replyTo(said(inA, "alice-id", 1003, "lol"), 5000)} {
		if !a.claim(m).Done {
			t.Fatalf("%q not taken", m.Text)
		}
	}
	unaddressed := said(inA, "bob-id", 1004, "Rome")
	unaddressed.Quote = &feature.Quote{Bot: true, Sent: 5000}
	notBot := to(said(inA, "bob-id", 1005, "Rome"))
	notBot.Quote = &feature.Quote{Sent: 5000}
	for _, m := range []feature.Message{
		replyTo(said(inA, "bob-id", 1006, "Rome"), 7000), // another of the bot's messages
		to(said(inA, "bob-id", 1007, "Rome")),            // no reply
		said(inA, "bob-id", 1008, "Rome"),
		unaddressed,
		notBot,
		replyTo(said(inB, "bob-id", 1009, "Rome"), 6000),
	} {
		if !a.claim(m).IsZero() {
			t.Fatalf("taken: %+v", m)
		}
	}

	stamps := make([]int64, 100)
	for i := range stamps {
		stamps[i] = int64(10000 + i)
	}
	a.replied(inA, stamps)
	s := a.state()
	if len(s.Watches) != 1 || len(s.Watches[0].Sent) != maxSent || s.Watches[0].Sent[0] != 10036 || !s.Watches[0].Replies {
		t.Fatalf("saved %+v", s.Watches)
	}
	if !a.claim(replyTo(said(inA, "bob-id", 1010, "Rome"), 10099)).Done || !a.claim(replyTo(said(inA, "bob-id", 1011, "Rome"), 5000)).IsZero() {
		t.Fatal("not the last maxSent")
	}

	b, later := unitAddon(everything) // the bot started again
	*later = *now
	b.restore(s)
	if !b.claim(replyTo(said(inA, "bob-id", 1012, "Oslo"), 10050)).Done {
		t.Fatal("not taken after a restart")
	}
	*later = later.Add(time.Minute)
	if !b.claim(replyTo(said(inA, "bob-id", 1013, "Oslo"), 10050)).IsZero() {
		t.Fatal("taken after the watch ended")
	}

	c, _ := unitAddon(everything)
	*now = time.Unix(1800000000, 0)
	c.now = func() time.Time { return *now }
	if c.watch(cmd, replies("p", `(?i)^\s*[a-d]\s*$`)) == nil {
		t.Fatalf("a pattern refused: %s", log.String())
	}
	c.replied(inA, []int64{5000})
	if !c.claim(replyTo(said(inA, "bob-id", 1014, " B "), 5000)).Done || !c.claim(replyTo(said(inA, "bob-id", 1015, "Paris"), 5000)).IsZero() {
		t.Fatal("the pattern wasn't applied")
	}
}

// Through the addon package: the command's answer and the addon's post
// both take replies, which go to the watch's tool, and the bot started
// again still knows them.
func TestPackageAddonReplies(t *testing.T) {
	dir := t.TempDir()
	start := func() *featuretest.Host {
		h, _, err := register(t, quizConfig(""), func(h *featuretest.Host) { h.Settings["STATE_DIRECTORY"] = dir })
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	h := start()
	ctx := context.Background()
	act := h.Message(ctx, in(inA, "/round"))
	if act.Answer != "Question 1" || act.OnSent == nil {
		t.Fatalf("round: %+v", act)
	}
	act.OnSent([]int64{5000}, false)
	eventually(t, "the post", func() bool { return len(h.Posts()) == 1 })
	h.Posts()[0].OnSent([]int64{6000}, false)
	for i, sent := range []int64{5000, 6000} {
		if got := h.Message(ctx, replyTo(said(inA, "bob-id", int64(1001+i), "Paris"), sent)); !got.Done {
			t.Fatalf("a reply to %d: %+v", sent, got)
		}
	}
	eventually(t, "the reactions", func() bool { return len(h.Reactions()) == 2 })
	if got := h.Message(ctx, replyTo(said(inA, "bob-id", 1003, "Paris"), 7000)); !got.IsZero() {
		t.Fatalf("a reply to another message: %+v", got)
	}
	h.Stop()

	h = start()
	if got := h.Message(ctx, replyTo(said(inA, "bob-id", 1004, "Rome"), 6000)); !got.Done {
		t.Fatalf("after a restart: %+v", got)
	}
	eventually(t, "the reaction", func() bool { return len(h.Reactions()) == 1 })
}

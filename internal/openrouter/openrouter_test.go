package openrouter

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/chats"
	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

const secret = "Alice's secret text"

// served is one scripted answer: a status (200 if 0) and a body. A body
// of "hangup" drops the connection instead.
type served struct {
	status int
	body   string
}

// model is a fake chat completions API. It gives its answers in turn,
// repeating the last, and records each request.
type model struct {
	mu      sync.Mutex
	answers []served
	bodies  []map[string]any
	auth    string
}

func (m *model) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var body map[string]any
	json.Unmarshal(b, &body)
	m.mu.Lock()
	m.bodies = append(m.bodies, body)
	m.auth = r.Header.Get("Authorization")
	a := m.answers[min(len(m.bodies), len(m.answers))-1]
	m.mu.Unlock()
	if a.body == "hangup" {
		c, _, _ := w.(http.Hijacker).Hijack()
		c.Close()
		return
	}
	w.WriteHeader(max(a.status, 200))
	io.WriteString(w, a.body)
}

func (m *model) authorization() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.auth
}

func (m *model) requests() []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.bodies
}

func text(s string) served {
	return served{body: `{"choices":[{"message":{"content":` + quote(s) + `}}],"usage":{"prompt_tokens":20,"completion_tokens":3,"cost":0.002}}`}
}

func calls(names ...string) served {
	var cs []string
	for i, n := range names {
		cs = append(cs, fmt.Sprintf(`{"id":"c%d","type":"function","function":{"name":%q,"arguments":%s}}`, i, n, quote(`{"q":"`+secret+`"}`)))
	}
	return served{body: `{"choices":[{"message":{"content":"","tool_calls":[` + strings.Join(cs, ",") + `]}}],"usage":{"prompt_tokens":10,"completion_tokens":2,"cost":0.001}}`}
}

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

func start(t *testing.T, answers ...served) (*Client, *model, *logx.Buffer) {
	t.Helper()
	var logs logx.Buffer
	t.Cleanup(logx.SetOutput(&logs, &logs, false))
	retryDelay = time.Millisecond
	m := &model{answers: answers}
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	c := New(&config.Config{APIKey: "or-key", Model: "x/model", Reasoning: "low", MaxConcurrency: 8})
	c.URL, c.ModelsURL = srv.URL, srv.URL
	return c, m, &logs
}

func TestNewClient(t *testing.T) {
	c := New(&config.Config{MaxConcurrency: 40})
	tr := c.http.Transport.(*http.Transport)
	if tr.MaxIdleConnsPerHost != 40 || tr.TLSClientConfig.MinVersion != tls.VersionTLS13 {
		t.Fatalf("idle %d, TLS %x", tr.MaxIdleConnsPerHost, tr.TLSClientConfig.MinVersion)
	}
}

func TestRequestAndReply(t *testing.T) {
	c, m, logs := start(t, served{body: `{"choices":[{"message":{"content":" hi there "}}],` +
		`"usage":{"prompt_tokens":100,"completion_tokens":20,"cost":0.0042,"prompt_tokens_details":{"cached_tokens":40}}}`})
	r, err := c.Ask(context.Background(), Request{System: "be brief", Text: "hello", Images: []string{"data:image/png;base64,AA=="},
		History: []chats.Entry{{Role: "user", Content: "Bob: earlier"}, {Role: "assistant", Content: "ok"}}})
	if err != nil || r.Text != "hi there" || r.Pictures != nil || r.ToolsUsed != nil {
		t.Fatalf("got %+v, %v", r, err)
	}
	b := m.requests()[0]
	if m.authorization() != "Bearer or-key" || b["model"] != "x/model" {
		t.Errorf("auth %q, model %v", m.auth, b["model"])
	}
	got, _ := json.Marshal(b["messages"])
	want := `[{"content":"be brief","role":"system"},{"content":"Bob: earlier","role":"user"},{"content":"ok","role":"assistant"},` +
		`{"content":[{"text":"hello","type":"text"},{"image_url":{"url":"data:image/png;base64,AA=="},"type":"image_url"}],"role":"user"}]`
	if string(got) != want {
		t.Errorf("messages\n got %s\nwant %s", got, want)
	}
	if fmt.Sprint(b["provider"]) != "map[data_collection:deny]" || fmt.Sprint(b["usage"]) != "map[include:true]" ||
		fmt.Sprint(b["reasoning"]) != "map[effort:low]" {
		t.Errorf("options %v %v %v", b["provider"], b["usage"], b["reasoning"])
	}
	for _, k := range []string{"tools", "tool_choice", "plugins"} {
		if _, ok := b[k]; ok {
			t.Errorf("%s sent with no tools", k)
		}
	}
	if l := logs.String(); !strings.Contains(l, "tokens in/out: 100/20 (40% cached), $0.004") {
		t.Errorf("cost not logged: %q", l)
	}
}

func TestReasoningNone(t *testing.T) {
	c, m, _ := start(t, text("ok"))
	c.reasoning = "none"
	if _, err := c.Ask(context.Background(), Request{Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.requests()[0]["reasoning"]; ok {
		t.Error("reasoning sent")
	}
}

func TestRetries(t *testing.T) {
	moderation := `{"error":{"message":"Input flagged","metadata":{"flagged_input":"` + secret + `"}}}`
	for _, tc := range []struct {
		name    string
		answers []served
		reqs    int
		err     string
	}{
		{"5xx then ok", []served{{status: 502, body: "x"}, text("ok")}, 2, ""},
		{"429 then ok", []served{{status: 429, body: "x"}, text("ok")}, 2, ""},
		{"network error then ok", []served{{body: "hangup"}, text("ok")}, 2, ""},
		{"5xx twice", []served{{status: 503, body: `{"error":{"message":"down"}}`}}, 2, "OpenRouter status 503: down"},
		{"4xx is not retried", []served{{status: 400, body: moderation}}, 1, "OpenRouter status 400: Input flagged"},
		{"unreadable error", []served{{status: 401, body: "nope"}}, 1, "OpenRouter status 401: (unreadable error body)"},
		{"empty", []served{{body: `{"choices":[{"message":{"content":"  "}}]}`}}, 1, "empty response"},
		{"no choices", []served{{body: `{"choices":[]}`}}, 1, "empty response"},
		{"malformed", []served{{body: `not json ` + secret}}, 1, "malformed response"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, m, logs := start(t, tc.answers...)
			r, err := c.Ask(context.Background(), Request{Text: "hi"})
			if n := len(m.requests()); n != tc.reqs {
				t.Errorf("%d requests, want %d", n, tc.reqs)
			}
			if tc.err == "" && (err != nil || r.Text != "ok") || tc.err != "" && (err == nil || err.Error() != tc.err) {
				t.Errorf("got %+v, %v", r, err)
			}
			if err != nil && strings.Contains(err.Error()+logs.String(), secret) {
				t.Errorf("the request leaked into %q", err)
			}
		})
	}
}

// The model calls a tool, gets its result and answers. Neither the
// arguments nor the result reach the log; the cost is summed.
func TestToolLoop(t *testing.T) {
	c, m, logs := start(t, calls("echo"), text("done"))
	var got string
	echo := feature.Tool{Name: "echo", Description: "echoes", Schema: json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
		Run: func(_ context.Context, args string) (string, []feature.Picture, error) {
			got = args
			return "echo " + args, nil, nil
		}}
	server := feature.Tool{Server: "openrouter:server_tool"}
	r, err := c.Ask(context.Background(), Request{Text: "hi", Tools: []feature.Tool{server, echo}})
	if err != nil || r.Text != "done" || fmt.Sprint(r.ToolsUsed) != "[echo]" {
		t.Fatalf("got %+v, %v", r, err)
	}
	if got != `{"q":"`+secret+`"}` {
		t.Errorf("Run got %q", got)
	}
	reqs := m.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests", len(reqs))
	}
	tools, _ := json.Marshal(reqs[0]["tools"])
	if string(tools) != `[{"type":"openrouter:server_tool"},{"function":{"description":"echoes","name":"echo","parameters":{"properties":{"q":{"type":"string"}},"type":"object"}},"type":"function"}]` {
		t.Errorf("tools %s", tools)
	}
	msgs := reqs[1]["messages"].([]any)
	call, answer := msgs[len(msgs)-2].(map[string]any), msgs[len(msgs)-1].(map[string]any)
	if call["role"] != "assistant" || call["tool_calls"] == nil {
		t.Errorf("the call was not replayed: %v", call)
	}
	if answer["role"] != "tool" || answer["tool_call_id"] != "c0" || answer["content"] != `echo {"q":"`+secret+`"}` {
		t.Errorf("the result was not sent back: %v", answer)
	}
	l := logs.String()
	if !strings.Contains(l, "tool echo ") || !strings.Contains(l, "$0.003") || strings.Contains(l, secret) {
		t.Errorf("logged %q", l)
	}
}

// A ToolChoice is required until its result is in, and only for a function
// tool the request offers.
func TestToolChoice(t *testing.T) {
	c, m, _ := start(t, calls("quote"), text("done"))
	quote := feature.Tool{Name: "quote", Run: func(context.Context, string) (string, []feature.Picture, error) { return "230.10", nil, nil }}
	if _, err := c.Ask(context.Background(), Request{Text: "/quote", Tools: []feature.Tool{quote}, ToolChoice: "quote"}); err != nil {
		t.Fatal(err)
	}
	reqs := m.requests()
	if fmt.Sprint(reqs[0]["tool_choice"]) != "map[function:map[name:quote] type:function]" || reqs[1]["tool_choice"] != nil {
		t.Errorf("tool_choice %v then %v", reqs[0]["tool_choice"], reqs[1]["tool_choice"])
	}
	if p, _ := json.Marshal(reqs[0]["tools"]); !strings.Contains(string(p), `"parameters":{"type":"object"}`) {
		t.Errorf("a tool with no schema needs an empty object: %s", p)
	}

	c, m, _ = start(t, text("ok"))
	server := feature.Tool{Server: "openrouter:server_tool"}
	if _, err := c.Ask(context.Background(), Request{Text: "hi", Tools: []feature.Tool{server}, ToolChoice: "openrouter:server_tool"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.requests()[0]["tool_choice"]; ok {
		t.Error("a server tool was forced")
	}
}

func TestRoundCap(t *testing.T) {
	c, m, logs := start(t, calls("echo"))
	echo := feature.Tool{Name: "echo", Run: func(context.Context, string) (string, []feature.Picture, error) { return "x", nil, nil }}
	r, err := c.Ask(context.Background(), Request{Tools: []feature.Tool{echo}})
	if err != nil || r.Text != "Sorry, I couldn't finish that." || len(r.ToolsUsed) != maxRounds-1 {
		t.Fatalf("got %+v, %v", r, err)
	}
	if n := len(m.requests()); n != maxRounds {
		t.Fatalf("%d requests", n)
	}
	if !strings.Contains(logs.String(), "still calling tools after 5 requests") {
		t.Errorf("logged %q", logs.String())
	}
}

// Pictures are kept up to MaxPictures, and a reply of pictures alone is an
// answer.
func TestPictures(t *testing.T) {
	c, _, _ := start(t, calls("draw", "draw"), text(""))
	draw := feature.Tool{Name: "draw", Run: func(context.Context, string) (string, []feature.Picture, error) {
		return "drawn", []feature.Picture{{Caption: "a"}, {Caption: "b"}, {Caption: "c"}}, nil
	}}
	r, err := c.Ask(context.Background(), Request{Tools: []feature.Tool{draw}})
	if err != nil || len(r.Pictures) != feature.MaxPictures || r.Pictures[3].Caption != "a" || fmt.Sprint(r.ToolsUsed) != "[draw draw]" {
		t.Fatalf("got %+v, %v", r, err)
	}
}

func TestToolFailures(t *testing.T) {
	c, m, logs := start(t, calls("fails", "panics", "missing"), text("sorry"))
	fails := feature.Tool{Name: "fails", Run: func(context.Context, string) (string, []feature.Picture, error) { return "", nil, errors.New("boom") }}
	panics := feature.Tool{Name: "panics", Run: func(context.Context, string) (string, []feature.Picture, error) { panic(secret) }}
	r, err := c.Ask(context.Background(), Request{Tools: []feature.Tool{fails, panics}})
	if err != nil || r.Text != "sorry" || r.ToolsUsed != nil {
		t.Fatalf("got %+v, %v", r, err)
	}
	msgs := m.requests()[1]["messages"].([]any)
	var results []string
	for _, x := range msgs[len(msgs)-3:] {
		results = append(results, x.(map[string]any)["content"].(string))
	}
	if strings.Join(results, "|") != "error: boom|error: the tool failed|error: there is no such tool" {
		t.Errorf("results %q", results)
	}
	l := logs.String()
	if !strings.Contains(l, "tool fails failed") || !strings.Contains(l, "tool panics failed") || !strings.Contains(l, "wasn't offered") || strings.Contains(l, secret) {
		t.Errorf("logged %q", l)
	}
}

// The reply budget ends a slow request.
func TestBudget(t *testing.T) {
	c, _, _ := start(t, text("late"))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer slow.Close()
	defer close(release)
	c.URL = slow.URL
	if _, err := c.Ask(ctx, Request{Text: "hi"}); err == nil {
		t.Fatal("no error")
	}
}

func TestCapabilities(t *testing.T) {
	list := `{"data":[{"id":"x/model","architecture":{"input_modalities":["text","image"]},"supported_parameters":["tools","reasoning"]},` +
		`{"id":"x/plain","architecture":{"input_modalities":["text"]},"supported_parameters":[]}]}`
	for _, tc := range []struct {
		name, model   string
		answer        served
		images, tools bool
		warn          string
	}{
		{"listed", "x/model", served{body: list}, true, true, ""},
		{"listed without", "x/plain", served{body: list}, false, false, ""},
		{"not listed", "x/other", served{body: list}, false, true, "not in OpenRouter's model list"},
		{"failed", "x/model", served{status: 500, body: "x"}, false, true, "status 500"},
		{"unreachable", "x/model", served{body: "hangup"}, false, true, "images are off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, m, logs := start(t, tc.answer)
			c.model = tc.model
			images, tools := c.Capabilities(context.Background())
			if images != tc.images || tools != tc.tools {
				t.Errorf("images %v, tools %v", images, tools)
			}
			if l := logs.String(); tc.warn == "" && l != "" || !strings.Contains(l, tc.warn) {
				t.Errorf("logged %q", l)
			}
			if a := m.authorization(); a != "Bearer or-key" {
				t.Errorf("auth %q", a)
			}
		})
	}
}

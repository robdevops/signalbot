package model

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

// start is a client of a fake OpenRouter that gives answers.
func start(t *testing.T, answers ...served) (*Client, *model, *logx.Buffer) {
	t.Helper()
	return startAt(t, true, answers...)
}

// startAt is a client of a fake API, OpenRouter's if openRouter, that gives
// answers.
func startAt(t *testing.T, openRouter bool, answers ...served) (*Client, *model, *logx.Buffer) {
	t.Helper()
	var logs logx.Buffer
	t.Cleanup(logx.SetOutput(&logs, &logs, false))
	old := retryDelay
	retryDelay = time.Millisecond
	t.Cleanup(func() { retryDelay = old })
	m := &model{answers: answers}
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	return client(t, srv.URL, openRouter), m, &logs
}

// client is a client of the API at base, OpenRouter's if openRouter.
func client(t *testing.T, base string, openRouter bool) *Client {
	cfg := &config.Config{APIURL: base, APIKey: "or-key", Model: "x/model", Reasoning: "low", MaxConcurrency: 8}
	if u, _ := url.Parse(base); openRouter {
		cfg.Test = &config.Test{OpenRouter: u.Hostname()}
	}
	return New(cfg)
}

func TestNewClient(t *testing.T) {
	c := New(&config.Config{APIURL: "https://openrouter.ai/api/v1", MaxConcurrency: 40})
	tr := c.http.Transport.(*http.Transport)
	if tr.MaxIdleConnsPerHost != 40 || tr.TLSClientConfig.MinVersion != tls.VersionTLS13 {
		t.Fatalf("idle %d, TLS %x", tr.MaxIdleConnsPerHost, tr.TLSClientConfig.MinVersion)
	}
	if !c.openRouter {
		t.Error("the default API is not OpenRouter's")
	}
	for _, u := range []string{"https://OpenRouter.ai/api/v1", "https://openrouter.ai:443/api/v1"} {
		if c := New(&config.Config{APIURL: u}); !c.openRouter {
			t.Errorf("%s is not taken for OpenRouter", u)
		}
	}
	for _, u := range []string{"http://localhost:11434/v1", "https://api.openai.com/v1", "https://openrouter.ai.example.com/api/v1", "::bad"} {
		if c := New(&config.Config{APIURL: u}); c.openRouter {
			t.Errorf("%s is taken for OpenRouter", u)
		}
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
	if _, ok := b["reasoning_effort"]; ok {
		t.Error("reasoning_effort sent to OpenRouter, which takes the reasoning object")
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
		{"5xx twice", []served{{status: 503, body: `{"error":{"message":"down"}}`}}, 2, "status 503: down"},
		{"4xx is not retried", []served{{status: 400, body: moderation}}, 1, "status 400: Input flagged"},
		{"unreadable error", []served{{status: 401, body: "nope"}}, 1, "status 401: (unreadable error body)"},
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
	start(t) // for the logs
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	release := make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer slow.Close()
	defer close(release)
	c := client(t, slow.URL, true)
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
		{"unreachable", "x/model", served{body: "hangup"}, false, true, "images and audio are off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, m, logs := start(t, tc.answer)
			c.model = tc.model
			caps := c.Capabilities(context.Background())
			if caps.Images != tc.images || caps.Tools != tc.tools || !caps.Files {
				t.Errorf("caps %+v", caps)
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

// Another API gets a plain request: no OpenRouter extras, reasoning as the
// standard reasoning_effort, and no key when there is none.
func TestOtherAPIRequest(t *testing.T) {
	c, m, logs := startAt(t, false, text("hi"))
	c.key = ""
	if r, err := c.Ask(context.Background(), Request{System: "be brief", Text: "hello"}); err != nil || r.Text != "hi" {
		t.Fatalf("got %+v, %v", r, err)
	}
	b := m.requests()[0]
	for _, k := range []string{"provider", "usage", "reasoning", "tools", "tool_choice"} {
		if _, ok := b[k]; ok {
			t.Errorf("%s sent: %v", k, b[k])
		}
	}
	if b["reasoning_effort"] != "low" || b["model"] != "x/model" {
		t.Errorf("reasoning_effort %v, model %v", b["reasoning_effort"], b["model"])
	}
	if a := m.authorization(); a != "" {
		t.Errorf("auth %q with no key", a)
	}
	if l := logs.String(); !strings.Contains(l, "reply ") {
		t.Errorf("logged %q", l)
	}

	c, m, _ = startAt(t, false, text("hi"))
	c.reasoning = "none"
	if _, err := c.Ask(context.Background(), Request{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.requests()[0]["reasoning_effort"]; ok {
		t.Error("reasoning_effort sent with none")
	}
	if a := m.authorization(); a != "Bearer or-key" {
		t.Errorf("auth %q", a)
	}
}

// The cost is logged only when the API reports it.
func TestCostOnlyWhenReported(t *testing.T) {
	c, _, logs := startAt(t, false, served{body: `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":20,"completion_tokens":3}}`})
	if _, err := c.Ask(context.Background(), Request{Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	if l := logs.String(); !strings.Contains(l, "tokens in/out: 20/3") || strings.Contains(l, "$") {
		t.Errorf("logged %q", l)
	}
}

// A server tool is usable only on the provider its prefix names; any other
// is left out with a warning naming it. Function tools are always usable.
func TestUsable(t *testing.T) {
	fn := feature.Tool{Name: "echo", Run: func(context.Context, string) (string, []feature.Picture, error) { return "", nil, nil }}
	search, other := feature.Tool{Server: "openrouter:server_tool"}, feature.Tool{Server: "elsewhere:server_tool"}

	c, _, logs := start(t)
	if got := c.Usable([]feature.Tool{search, fn, other}); len(got) != 2 || got[0].Server != search.Server || got[1].Name != "echo" {
		t.Errorf("on OpenRouter got %+v", got)
	}
	if l := logs.String(); l != "tool elsewhere:server_tool left out: it runs only on elsewhere's API\n" {
		t.Errorf("logged %q", l)
	}

	c, _, logs = startAt(t, false)
	if got := c.Usable([]feature.Tool{search, fn}); len(got) != 1 || got[0].Name != "echo" {
		t.Errorf("elsewhere got %+v", got)
	}
	if l := logs.String(); l != "tool openrouter:server_tool left out: it runs only on openrouter's API\n" {
		t.Errorf("logged %q", l)
	}
}

// Only OpenRouter has a model list: elsewhere images and tools are assumed,
// with one startup line and no request.
func TestOtherAPICapabilities(t *testing.T) {
	c, m, logs := startAt(t, false, served{status: 500})
	if caps := c.Capabilities(context.Background()); caps != (Caps{Images: true, Audio: true, Files: true, Tools: true}) {
		t.Errorf("caps %+v", caps)
	}
	if n := len(m.requests()); n != 0 {
		t.Errorf("%d requests", n)
	}
	if l := logs.String(); strings.Count(l, "\n") != 1 || !strings.Contains(l, "images, files and tools are on until the API rejects them") {
		t.Errorf("logged %q", l)
	}
}

// Elsewhere, a first request refused with a 4xx about the request is
// ErrImages if it carried images, else ErrTools if it carried tools, with
// no retry. A refused key, credit or rate, a later request, a request with
// neither, and anything on OpenRouter keep their plain error.
func TestRejected(t *testing.T) {
	echo := feature.Tool{Name: "echo", Run: func(context.Context, string) (string, []feature.Picture, error) { return "done", nil, nil }}
	bad := served{status: 400, body: `{"error":{"message":"no images here"}}`}
	for _, tc := range []struct {
		name       string
		openRouter bool
		answers    []served
		r          Request
		want       error
		requests   int
	}{
		{"images", false, []served{bad}, Request{Text: "hi", Images: []string{"data:image/png;base64,AA=="}, Tools: []feature.Tool{echo}}, ErrImages, 1},
		{"files", false, []served{bad}, Request{Text: "hi", Files: []feature.File{{Name: "a.pdf", MIME: "application/pdf"}}, Tools: []feature.Tool{echo}}, ErrFiles, 1},
		{"tools", false, []served{bad}, Request{Text: "hi", Tools: []feature.Tool{echo}}, ErrTools, 1},
		{"neither", false, []served{bad}, Request{Text: "hi"}, nil, 1},
		{"key", false, []served{{status: 401, body: `{}`}}, Request{Text: "hi", Tools: []feature.Tool{echo}}, nil, 1},
		{"rate", false, []served{{status: 429, body: `{}`}}, Request{Text: "hi", Tools: []feature.Tool{echo}}, nil, 2},
		{"unprocessable", false, []served{{status: 422, body: `{}`}}, Request{Text: "hi", Tools: []feature.Tool{echo}}, ErrTools, 1},
		{"model id", false, []served{{status: 404, body: `{}`}}, Request{Text: "hi", Images: []string{"data:image/png;base64,AA=="}, Tools: []feature.Tool{echo}}, nil, 1},
		{"timeout", false, []served{{status: 408, body: `{}`}}, Request{Text: "hi", Images: []string{"data:image/png;base64,AA=="}, Tools: []feature.Tool{echo}}, nil, 1},
		{"too large", false, []served{{status: 413, body: `{}`}}, Request{Text: "hi", Images: []string{"data:image/png;base64,AA=="}}, nil, 1},
		{"later request", false, []served{calls("echo"), bad}, Request{Text: "hi", Tools: []feature.Tool{echo}}, nil, 2},
		{"openrouter", true, []served{bad}, Request{Text: "hi", Images: []string{"data:image/png;base64,AA=="}, Tools: []feature.Tool{echo}}, nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, m, _ := startAt(t, tc.openRouter, tc.answers...)
			_, err := c.Ask(context.Background(), tc.r)
			if err == nil || errors.Is(err, ErrImages) != (tc.want == ErrImages) || errors.Is(err, ErrTools) != (tc.want == ErrTools) ||
				errors.Is(err, ErrFiles) != (tc.want == ErrFiles) {
				t.Errorf("error %v, want %v", err, tc.want)
			}
			if n := len(m.requests()); n != tc.requests {
				t.Errorf("%d requests", n)
			}
		})
	}
}

// A PDF goes as a file part and audio as input_audio, after the text and
// images. On OpenRouter a PDF for a model that can't read files itself is
// read by pdfEngine; one that can, or another API, gets no plugin.
func TestFiles(t *testing.T) {
	pdf := feature.File{Name: "report.pdf", MIME: "application/pdf", Data: []byte("%PDF")}
	voice := feature.File{Name: "untitled", MIME: "audio/aac", Data: []byte("aac")}
	for _, tc := range []struct {
		name               string
		openRouter, native bool
		files              []feature.File
		plugin             bool
	}{
		{"non-native", true, false, []feature.File{pdf, voice}, true},
		{"native", true, true, []feature.File{pdf}, false},
		{"audio only", true, false, []feature.File{voice}, false},
		{"other API", false, false, []feature.File{pdf}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, m, _ := startAt(t, tc.openRouter, text("ok"))
			c.nativeFiles = tc.native
			if _, err := c.Ask(context.Background(), Request{Text: "read this", Files: tc.files}); err != nil {
				t.Fatal(err)
			}
			b := m.requests()[0]
			msgs := b["messages"].([]any)
			got, _ := json.Marshal(msgs[len(msgs)-1])
			want := `{"content":[{"text":"read this","type":"text"}`
			for _, f := range tc.files {
				if f.MIME == "audio/aac" {
					want += `,{"input_audio":{"data":"YWFj","format":"aac"},"type":"input_audio"}`
				} else {
					want += `,{"file":{"file_data":"data:application/pdf;base64,JVBERg==","filename":"report.pdf"},"type":"file"}`
				}
			}
			if want += `],"role":"user"}`; string(got) != want {
				t.Errorf("message\n got %s\nwant %s", got, want)
			}
			plugins, _ := json.Marshal(b["plugins"])
			if tc.plugin != (string(plugins) == `[{"id":"file-parser","pdf":{"engine":"mistral-ocr"}}]`) || !tc.plugin && b["plugins"] != nil {
				t.Errorf("plugins %s", plugins)
			}
		})
	}
	for mime, want := range map[string]string{"audio/mpeg": "mp3", "audio/x-wav": "wav", "audio/mp4": "m4a", "audio/ogg; codecs=opus": "ogg",
		"audio/flac": "flac", "AUDIO/AAC": "aac", "application/pdf": ""} {
		if got := audioFormat(mime); got != want {
			t.Errorf("audioFormat(%q) = %q, want %q", mime, got, want)
		}
	}
}

// OpenRouter's annotations on a reply, such as a parsed PDF, go back with
// it in the next round, so the PDF isn't parsed again.
func TestAnnotationsSentBack(t *testing.T) {
	echo := feature.Tool{Name: "echo", Run: func(context.Context, string) (string, []feature.Picture, error) { return "done", nil, nil }}
	withNotes := served{body: `{"choices":[{"message":{"content":"","annotations":[{"type":"file","file":{"hash":"h1"}}],` +
		`"tool_calls":[{"id":"c0","type":"function","function":{"name":"echo","arguments":"{}"}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`}
	c, m, logs := start(t, withNotes, text("ok"))
	if _, err := c.Ask(context.Background(), Request{Text: "read", Tools: []feature.Tool{echo}}); err != nil {
		t.Fatal(err)
	}
	msgs := m.requests()[1]["messages"].([]any)
	got, _ := json.Marshal(msgs[2])
	if !strings.Contains(string(got), `"annotations":[{"file":{"hash":"h1"},"type":"file"}]`) {
		t.Fatalf("assistant message %s", got)
	}
	// Each request is logged at debug level with what it cost and whether
	// annotations came back and went back, never the content.
	for _, want := range []string{"request 1: tokens in/out 10/2, no cost reported, annotations returned: yes, sent back: no",
		"request 2: tokens in/out 20/3, $0.0020, annotations returned: no, sent back: yes"} {
		if !strings.Contains(logs.String(), want) {
			t.Errorf("log %q, want %q", logs.String(), want)
		}
	}
}

// The model list says whether the model takes audio and reads files itself.
func TestCapabilitiesAudioAndFiles(t *testing.T) {
	c, _, _ := start(t, served{body: `{"data":[{"id":"x/model","architecture":{"input_modalities":["text","image","audio","file"]},"supported_parameters":["tools"]}]}`})
	if caps := c.Capabilities(context.Background()); caps != (Caps{Images: true, Audio: true, Files: true, Tools: true}) || !c.nativeFiles {
		t.Fatalf("caps %+v, native %v", caps, c.nativeFiles)
	}
	c, _, _ = start(t, served{body: `{"data":[{"id":"x/model","architecture":{"input_modalities":["text"]},"supported_parameters":[]}]}`})
	if caps := c.Capabilities(context.Background()); caps != (Caps{Files: true}) || c.nativeFiles {
		t.Fatalf("caps %+v, native %v", caps, c.nativeFiles)
	}
}

// A reply that fails after a request was billed still reports that cost,
// so a daily budget counts it.
func TestCostOnError(t *testing.T) {
	echo := feature.Tool{Name: "echo", Run: func(context.Context, string) (string, []feature.Picture, error) { return "done", nil, nil }}
	c, _, _ := start(t, calls("echo"), served{status: 500, body: `{}`})
	r, err := c.Ask(context.Background(), Request{Text: "hi", Tools: []feature.Tool{echo}})
	if err == nil || !r.Costed || r.Cost != 0.001 {
		t.Fatalf("cost %v (%v), %v", r.Cost, r.Costed, err)
	}
}

// The retry wait is a second, unless a test hook sets one.
func TestRetryDelay(t *testing.T) {
	if retryDelay != time.Second {
		t.Fatalf("retry delay %v", retryDelay)
	}
	if c := New(&config.Config{Test: &config.Test{RetryDelay: time.Millisecond}}); c.retry != time.Millisecond {
		t.Fatalf("hook ignored: %v", c.retry)
	}
}

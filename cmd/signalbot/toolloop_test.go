package main

import (
	"context"
	"encoding/json"
	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/llm"
	"github.com/robdevops/signalbot/internal/llm/openai"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/kit/logx/logxtest"

	"github.com/robdevops/signalbot/internal/transports/signal"
)

// fakeModel answers chat completions: with a call to fake__echo for the first
// calls replies, then with "done". It records every request body.
type fakeModel struct {
	mu     sync.Mutex
	calls  int // replies that call a tool before answering
	bodies []map[string]any
}

func (f *fakeModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	var body map[string]any
	json.Unmarshal(b, &body)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bodies = append(f.bodies, body)
	if len(f.bodies) <= f.calls {
		args, _ := json.Marshal(`{"q":"` + secretText + `"}`)
		io.WriteString(w, `{"choices":[{"message":{"content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"fake__echo","arguments":`+string(args)+`}}]}}],"usage":{"prompt_tokens":10,"completion_tokens":2,"cost":0.001}}`)
		return
	}
	io.WriteString(w, `{"choices":[{"message":{"content":"done"}}],"usage":{"prompt_tokens":20,"completion_tokens":3,"cost":0.002}}`)
}

// toolBot is a bot with web search and a fake MCP server's echo tool,
// asking the fake model f. Its replies go nowhere: there is no socket.
func toolBot(t *testing.T, f *fakeModel) *bot.Bot {
	t.Helper()
	model := httptest.NewServer(f)
	t.Cleanup(model.Close)
	cfg := testCfg()
	cfg.Search = true
	cfg.MCPConfig = mcpServers(t, map[string][]string{"fake": {"echo"}}, nil)
	sig := signal.NewAdapter("x", nil)
	sig.Client().SetSelf(self)
	return NewBot(context.Background(), cfg, testModel(model.URL), sig)
}

// wait waits until the model has had n requests.
func (f *fakeModel) wait(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		got := len(f.bodies)
		f.mu.Unlock()
		if got >= n || time.Now().After(deadline) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// testModel is the tool loop over an OpenAI-compatible client at url.
func testModel(url string) llm.Loop {
	return llm.Loop{Model: openai.New(openai.Options{URL: url, Key: "k", Model: "m"})}
}

// The model calls a tool, gets its result, and answers; usage is summed
// over both requests. Neither the arguments nor the result reach the log.
func TestToolCallLoop(t *testing.T) {
	f := &fakeModel{calls: 1}
	b := toolBot(t, f)
	logs := logxtest.Capture(t)
	deliver(b, parse(t, groupMention))
	f.wait(t, 2)
	waitLog(t, logs, "reply Alice")
	if !strings.Contains(logs.String(), "tokens in/out: 30/5 (0% cached), $0.003") {
		t.Errorf("usage not summed over both requests: %q", logs.String())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) != 2 {
		t.Fatalf("%d requests, want 2", len(f.bodies))
	}
	sent := f.bodies[0]["tools"].([]any)
	if len(sent) != 2 || sent[0].(map[string]any)["type"] != "openrouter:web_search" ||
		sent[1].(map[string]any)["function"].(map[string]any)["name"] != "fake__echo" {
		t.Errorf("tools offered: %v", sent)
	}
	msgs := f.bodies[1]["messages"].([]any)
	call := msgs[len(msgs)-2].(map[string]any)
	answer := msgs[len(msgs)-1].(map[string]any)
	if call["role"] != "assistant" || call["tool_calls"] == nil {
		t.Errorf("assistant tool call not replayed: %v", call)
	}
	if answer["role"] != "tool" || answer["tool_call_id"] != "c1" || !strings.Contains(answer["content"].(string), secretText) {
		t.Errorf("tool result not sent back: %v", answer)
	}
	if !strings.Contains(logs.String(), "tool echo (fake)") {
		t.Errorf("tool call not logged: %q", logs.String())
	}
	if strings.Contains(logs.String(), secretText) {
		t.Errorf("tool arguments or result reached the log: %q", logs.String())
	}
}

// waitLog waits for want to appear in logs.
func waitLog(t *testing.T, logs interface{ String() string }, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), want) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}

// A model that keeps calling tools is cut off after llm.MaxRounds requests.
func TestToolCallRoundCap(t *testing.T) {
	f := &fakeModel{calls: 100}
	b := toolBot(t, f)
	logs := logxtest.Capture(t)
	deliver(b, parse(t, groupMention))
	waitLog(t, logs, "reply Alice")
	if !strings.Contains(logs.String(), "model still calling tools after 5 requests; stopping") {
		t.Fatalf("log %q", logs.String())
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) != llm.MaxRounds {
		t.Fatalf("%d requests, want %d", len(f.bodies), llm.MaxRounds)
	}
}

// The model calls the chart tool on a message that asks for a chart, and is
// told the chart's final values.
func TestChartToolInLoop(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(b, &body)
		mu.Lock()
		defer mu.Unlock()
		bodies = append(bodies, body)
		if len(bodies) == 1 {
			args, _ := json.Marshal(sampleChart)
			io.WriteString(w, `{"choices":[{"message":{"content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"make_chart","arguments":`+string(args)+`}}]}}]}`)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"Done"}}]}`)
	}))
	defer srv.Close()
	sig := signal.NewAdapter("x", nil)
	sig.Client().SetSelf(self)
	b := NewBot(context.Background(), testCfg(), testModel(srv.URL), sig)
	logs := logxtest.Capture(t)
	deliver(b, parse(t, strings.Replace(groupMention, "what is up", "chart NVDA", 1)))
	waitLog(t, logs, "reply Alice")
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("%d requests", len(bodies))
	}
	msgs := bodies[1]["messages"].([]any)
	if got := msgs[len(msgs)-1].(map[string]any)["content"].(string); !strings.HasPrefix(got, "Chart attached.") || !strings.Contains(got, "Final values: NVDA ") {
		t.Fatalf("tool result %q", got)
	}
	if !strings.Contains(logs.String(), "tool make_chart") {
		t.Fatalf("log %q", logs.String())
	}
}

const sampleChart = `{"title":"NVDA vs AMD, 1 month","series":[{"name":"NVDA","points":[{"date":"2026-10-01","value":180},{"date":"2026-10-28","value":190}]}]}`

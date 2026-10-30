package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
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

func modelWithTools(t *testing.T, f *fakeModel) *OpenRouter {
	t.Helper()
	m, _ := startFake(t, &fakeMCP{}, "echo")
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	o := NewOpenRouter(&Config{APIKey: "k", Model: "m", Search: true}, m)
	o.url = srv.URL
	return o
}

// The model calls a tool, gets its result, and answers. Neither the
// arguments nor the result reach the log.
func TestToolCallLoop(t *testing.T) {
	f := &fakeModel{calls: 1}
	o := modelWithTools(t, f)
	logs := captureLogsSafely(t)
	res, err := o.Complete(context.Background(), "s", []ChatMsg{{Role: "user", Content: "hi"}})
	if err != nil || res.Text != "done" {
		t.Fatalf("got %+v, %v", res, err)
	}
	if res.InputTokens != 30 || res.OutputTokens != 5 || res.Cost < 0.003-1e-9 || res.Cost > 0.003+1e-9 {
		t.Errorf("usage not summed over both requests: %+v", res)
	}
	if len(f.bodies) != 2 {
		t.Fatalf("%d requests, want 2", len(f.bodies))
	}
	tools := f.bodies[0]["tools"].([]any)
	if len(tools) != 2 || tools[0].(map[string]any)["type"] != "openrouter:web_search" ||
		tools[1].(map[string]any)["function"].(map[string]any)["name"] != "fake__echo" {
		t.Errorf("tools offered: %v", tools)
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

// A model that keeps calling tools is cut off after maxToolRounds requests.
func TestToolCallRoundCap(t *testing.T) {
	f := &fakeModel{calls: 100}
	o := modelWithTools(t, f)
	res, err := o.Complete(context.Background(), "s", nil)
	if err != nil || res.Text != "Sorry, I couldn't finish that." {
		t.Fatalf("got %+v, %v", res, err)
	}
	if len(f.bodies) != maxToolRounds {
		t.Fatalf("%d requests, want %d", len(f.bodies), maxToolRounds)
	}
}

// failAI fails the test if the model is asked anything.
type failAI struct{ t *testing.T }

func (f failAI) Complete(context.Context, string, []ChatMsg) (Completion, error) {
	f.t.Error("the model was called")
	return Completion{Text: "x"}, nil
}

// "/demo" is answered without the model, with a real mention of the sender in
// a group, and leaves no history.
func TestDemo(t *testing.T) {
	sent := make(chan map[string]any, 2)
	sock := fakeSignal(t, func(_ int, c net.Conn, r *bufio.Reader) {
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var req map[string]any
			json.Unmarshal(line, &req)
			id, _ := json.Marshal(req["id"])
			switch req["method"] {
			case "listAccounts":
				c.Write([]byte(`{"jsonrpc":"2.0","result":[{"number":null,"aci":"` + self + `"}],"id":` + string(id) + "}\n"))
			case "listContacts":
				c.Write([]byte(`{"jsonrpc":"2.0","result":[{"uuid":"` + self + `","givenName":"Botty"}],"id":` + string(id) + "}\n"))
			case "send":
				sent <- req["params"].(map[string]any)
			}
		}
	})
	cfg := testCfg()
	cfg.Model, cfg.Search = "x/test-model", true
	sig := NewSignal(sock)
	b := NewBot(context.Background(), cfg, sig, failAI{t})
	b.toolNames = []string{"get_forecast"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sig.Run(ctx, b.Handle)
	sig.WaitSelf(5 * time.Second)

	recv := func() map[string]any {
		t.Helper()
		select {
		case p := <-sent:
			return p
		case <-time.After(5 * time.Second):
			t.Fatal("no reply")
			return nil
		}
	}
	b.Handle(parse(t, strings.Replace(groupMention, "what is up", "/demo", 1)))
	p := recv()
	msg := p["message"].(string)
	if !strings.HasPrefix(msg, "Hi @Alice! I'm Botty.") || !strings.Contains(msg, "Run test-model with web search, get_forecast") {
		t.Fatalf("message %q", msg)
	}
	if ms, _ := p["mention"].([]any); len(ms) != 1 || ms[0] != "3:6:"+alice {
		t.Fatalf("mention %v, want 3:6:%s", p["mention"], alice)
	}
	if h := b.getHistory("g:" + group); len(h) != 0 {
		t.Fatalf("/demo left history: %v", h)
	}

	// In a DM there is no one to mention.
	cfg.AllowedSenders = map[string]bool{alice: true}
	b.Handle(parse(t, strings.Replace(dm, "Test DM", "/DEMO", 1)))
	if msg := recv()["message"].(string); !strings.HasPrefix(msg, "Hi! I'm Botty.") {
		t.Fatalf("DM message %q", msg)
	}
}

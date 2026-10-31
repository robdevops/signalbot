package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/kit/logx/logxtest"

	"github.com/robdevops/signalbot/internal/transports/signal"

	"github.com/robdevops/signalbot/internal/transports/signal/signaltest"
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
	logs := logxtest.Capture(t)
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

// "/example" is answered without the model, with a real mention of the sender in
// a group, and leaves no history.
func TestExample(t *testing.T) {
	sent := make(chan map[string]any, 2)
	sock := signaltest.Socket(t, func(_ int, c net.Conn, r *bufio.Reader) {
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
	sig := signal.NewAdapter(sock, cfg.AllowedSenders)
	b := NewBot(context.Background(), cfg, failAI{t}, sig)
	b.toolNames = []string{"get_forecast"}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)
	sig.Client().WaitSelf(5 * time.Second)

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
	deliver(b, parse(t, strings.Replace(groupMention, "what is up", "/example", 1)))
	p := recv()
	msg := p["message"].(string)
	if !strings.HasPrefix(msg, "Hi @Alice! I'm Botty.") || !strings.Contains(msg, "Run test-model with web search, charts, get_forecast") {
		t.Fatalf("message %q", msg)
	}
	if ms, _ := p["mention"].([]any); len(ms) != 1 || ms[0] != "3:6:"+alice {
		t.Fatalf("mention %v, want 3:6:%s", p["mention"], alice)
	}
	if h := b.getHistory("signal:g:" + group); len(h) != 0 {
		t.Fatalf("/example left history: %v", h)
	}

	// In a DM there is no one to mention.
	cfg.AllowedSenders[alice] = true
	deliver(b, parse(t, strings.Replace(dm, "Test DM", "/EXAMPLE", 1)))
	if msg := recv()["message"].(string); !strings.HasPrefix(msg, "Hi! I'm Botty.") {
		t.Fatalf("DM message %q", msg)
	}
}

// tinyPNG is a 1×1 PNG, base64-encoded.
func tinyPNG() string {
	var buf bytes.Buffer
	png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1)))
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// The model calls the chart tool; the chart comes back as an image for the
// reply, and the model is told the final values.
func TestChartToolInLoop(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(b, &body)
		bodies = append(bodies, body)
		if len(bodies) == 1 {
			args, _ := json.Marshal(sampleChart())
			io.WriteString(w, `{"choices":[{"message":{"content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"make_chart","arguments":`+string(args)+`}}]}}]}`)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"Here you go."}}]}`)
	}))
	defer srv.Close()
	o := NewOpenRouter(&Config{APIKey: "k", Model: "m"}, nil)
	o.url = srv.URL
	res, err := o.Complete(withChart(context.Background()), "s", nil)
	if err != nil || res.Text != "Here you go." || len(res.Images) != 1 {
		t.Fatalf("got text %q, %d images, %v", res.Text, len(res.Images), err)
	}
	if _, err := png.Decode(bytes.NewReader(res.Images[0])); err != nil {
		t.Fatalf("not a PNG: %v", err)
	}
	msgs := bodies[1]["messages"].([]any)
	if got := msgs[len(msgs)-1].(map[string]any)["content"].(string); !strings.HasPrefix(got, chartNote+"Final values: NVDA ") {
		t.Fatalf("tool result %q", got)
	}
}

// imageAI replies with text and one image, captioned if it is a chart.
type imageAI struct{ text, caption string }

func (a imageAI) Complete(context.Context, string, []ChatMsg) (Completion, error) {
	img, _ := base64.StdEncoding.DecodeString(tinyPNG())
	return Completion{Text: a.text, Images: [][]byte{img}, Captions: []string{a.caption}}, nil
}

// A reply is one message: a chart with its caption in place of the model's
// text, or an MCP image with the text, or the image alone.
func TestReplyCarriesImages(t *testing.T) {
	for _, tc := range []struct{ text, caption, want string }{
		{"Done", "NVDA vs AMD", "NVDA vs AMD"},
		{"look", "", "look"},
		{"", "", ""},
	} {
		text := tc.text
		sent := make(chan map[string]any, 2)
		sock := signaltest.Socket(t, func(_ int, c net.Conn, r *bufio.Reader) {
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
		sig := signal.NewAdapter(sock, nil)
		b := NewBot(context.Background(), testCfg(), imageAI{text, tc.caption}, sig)
		ctx, cancel := context.WithCancel(context.Background())
		go b.Run(ctx)
		sig.Client().WaitSelf(5 * time.Second)
		deliver(b, parse(t, groupMention))
		next := func() map[string]any {
			select {
			case p := <-sent:
				return p
			case <-time.After(5 * time.Second):
				t.Fatalf("text %q: no reply", text)
				return nil
			}
		}
		p := next()
		att, _ := p["attachments"].([]any)
		if p["message"] != tc.want || len(att) != 1 || !strings.HasPrefix(att[0].(string), "data:image/png;filename=image1.png;base64,") {
			t.Errorf("text %q: message %q, attachments %v", text, p["message"], att)
		}
		select {
		case p := <-sent:
			t.Errorf("text %q: a second message %v", text, p)
		case <-time.After(200 * time.Millisecond):
		}
		cancel()
	}
}

func TestWantsChart(t *testing.T) {
	for text, want := range map[string]bool{
		"chart NVDA vs AMD": true, "Can you graph this?": true, "plot the yields": true,
		"visualise it": true, "VISUALIZATION please": true, "charts?": true, "plotted over time": true,
		"a pie chart": true, "donut please": true, "doughnut": true, "pied": false, "spied on": false,
		"what is up": false, "a plotless film": false, "paragraph": false,
	} {
		if got := wantsChart(text); got != want {
			t.Errorf("%q: got %v, want %v", text, got, want)
		}
	}
}

// The chart tool goes only with a request that asks for a chart, and no tool
// at all goes to a model that can't call tools.
func TestToolListOffers(t *testing.T) {
	m, _ := startFake(t, &fakeMCP{}, "echo")
	o := NewOpenRouter(&Config{APIKey: "k", Model: "m", Search: true}, m)
	names := func(tools []any) (out []string) {
		for _, x := range tools {
			if f, ok := x.(map[string]any); ok {
				out = append(out, f["function"].(map[string]any)["name"].(string))
			} else {
				out = append(out, "search")
			}
		}
		return out
	}
	if got := strings.Join(names(o.toolList(context.Background())), ","); got != "search,fake__echo" {
		t.Errorf("no chart asked: %q", got)
	}
	if got := strings.Join(names(o.toolList(withChart(context.Background()))), ","); got != "search,make_chart,make_donut_chart,fake__echo" {
		t.Errorf("chart asked: %q", got)
	}
	o.noTools = true
	if got := o.toolList(withChart(context.Background())); got != nil {
		t.Errorf("a model without tool calls was offered %v", got)
	}
}

// The prompt names the chart tool only when it is offered, and no tools at
// all for a model that can't call them.
func TestSystemPromptTools(t *testing.T) {
	cfg := &Config{Model: "m", Search: true, MaxConcurrency: 1, HistoryTurns: 1}
	b := NewBot(context.Background(), cfg, nil, signal.NewAdapter("x", cfg.AllowedSenders))
	if p := b.prompt(false); !strings.Contains(p, "\nYour tools: web search. If asked") {
		t.Errorf("without a chart: %q", p)
	}
	if p := b.prompt(true); !strings.Contains(p, "\nYour tools: web search; charts (make_chart, make_donut_chart). If asked") {
		t.Errorf("with a chart: %q", p)
	}
	b.toolCalls = false
	if p := b.prompt(true); strings.Contains(p, "Your tools") || b.toolsText() != "none" {
		t.Errorf("a model without tool calls was told of tools: %q, %q", p, b.toolsText())
	}
}

// After a message asks for a chart or a reply draws one, the chart tools stay
// on for 2 more replies in that chat, for follow-ups.
func TestChartFollowUps(t *testing.T) {
	b := NewBot(context.Background(), testCfg(), nil, signal.NewAdapter("x", testCfg().AllowedSenders))
	reply := func(key, text string, drew bool) bool {
		asked := wantsChart(text)
		chart := asked || b.chartTurns(key) > 0
		if chart {
			b.chartReplied(key, asked || drew)
		}
		return chart
	}
	for i, step := range []struct {
		text string
		drew bool
		want bool
	}{
		{"chart NVDA for 3 months", true, true},
		{"show that for 1 month", false, true},
		{"and for a week", true, true}, // drew one: the count restarts
		{"thanks", false, true},
		{"what about AMD", false, true},
		{"ok", false, false},
	} {
		if got := reply("u:a", step.text, step.drew); got != step.want {
			t.Fatalf("step %d %q: chart tools %v, want %v", i, step.text, got, step.want)
		}
		if reply("u:other", "hello", false) {
			t.Fatal("another chat got the chart tools")
		}
	}
	reply("u:a", "plot it", false)
	b.mu.Lock()
	b.history["u:a"] = []ChatMsg{{Content: "old", Expires: time.Now().Add(-time.Second)}}
	b.mu.Unlock()
	b.forget("u:a")
	if b.chartTurns("u:a") != 0 {
		t.Fatal("erasing the history kept the chart tools on")
	}
}

// The history records the charts a reply sent, so the model knows it did.
func TestSentImagesNote(t *testing.T) {
	res := Completion{Images: [][]byte{{1}, {2}}, Captions: []string{"Apple revenue — Q3", ""}}
	if got := sentImages(res); got != "[sent the chart \"Apple revenue — Q3\"]\n[sent an image]\n" {
		t.Fatalf("got %q", got)
	}
}

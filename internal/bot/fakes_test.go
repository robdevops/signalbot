package bot

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

const (
	botID   = "00000000-0000-4000-8000-000000000000"
	aliceID = "11111111-1111-4111-8111-111111111111"
	bobID   = "22222222-2222-4222-8222-222222222222"
	groupID = "Z3JvdXA="
)

// harness is a bot wired to a fake signal-cli socket and a fake OpenRouter.
type harness struct {
	t    *testing.T
	b    *Bot
	logs *logx.Buffer

	mu      sync.Mutex
	conn    net.Conn               // the bot's connection, server side
	res     map[string]string      // signal-cli method: result JSON
	calls   []call                 // every signal-cli request
	asks    []map[string]any       // every model request body
	answers []func() (int, string) // queued model answers; then "ok"
	hold    chan struct{}          // while set, the model waits on it
	tools   bool                   // the model can call tools
	sent    chan map[string]any    // "send" params, in order
	id      chan struct{}          // if set, listAccounts waits until it is closed
}

type call struct {
	Method string
	Params map[string]any
}

// options for start.
type options struct {
	tools    bool
	waitID   bool // the bot's ID stays unknown until the test closes h.id
	features func(*Bot)
	cfg      func(*config.Config)
}

// start runs a bot on the fakes, connected and with its contacts loaded.
// Everything it started has stopped by the time the test ends.
func start(t *testing.T, o options) *harness {
	t.Helper()
	h := &harness{t: t, logs: &logx.Buffer{}, tools: o.tools, sent: make(chan map[string]any, 64), res: map[string]string{
		"listAccounts":     `[{"number":"+15550000","aci":"` + botID + `"}]`,
		"listContacts":     `[{"uuid":"` + botID + `","profile":{"givenName":"Botty"}},{"uuid":"` + aliceID + `","givenName":"Alice"},{"uuid":"` + bobID + `","givenName":"Bob"}]`,
		"listStickerPacks": `[]`,
		"listGroups":       `[{"id":"` + groupID + `","name":"Family","isMember":true,"members":[{"uuid":"` + botID + `"},{"uuid":"` + aliceID + `","number":"+1555"},{"uuid":"` + bobID + `"}]}]`,
	}}
	t.Cleanup(logx.SetOutput(h.logs, h.logs, true))
	if o.waitID {
		h.id = make(chan struct{})
	}

	sock := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			h.mu.Lock()
			h.conn = c
			h.mu.Unlock()
			go h.serve(c)
		}
	}()
	srv := httptest.NewServer(http.HandlerFunc(h.model))
	t.Cleanup(srv.Close)

	cfg := &config.Config{Socket: sock, APIKey: "k", Model: "test/test-model", Reasoning: "low", Intro: config.DefaultIntro,
		AllowedSenders: map[string]bool{aliceID: true}, MaxConcurrency: 4, HistoryTurns: 10}
	if o.cfg != nil {
		o.cfg(cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.b = New(ctx, cfg)
	h.b.ai.URL, h.b.ai.ModelsURL = srv.URL+"/chat/completions", srv.URL+"/models"
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
	if o.waitID {
		h.eventually("the bot to connect", func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.conn != nil })
	} else {
		h.eventually("the contacts to load", func() bool { return h.b.sig.BotName() == "Botty" })
	}
	return h
}

func (h *harness) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		json.Unmarshal(line, &req)
		h.mu.Lock()
		h.calls = append(h.calls, call{req.Method, req.Params})
		res, ok := h.res[req.Method]
		h.mu.Unlock()
		if req.Method == "send" {
			h.sent <- req.Params
		}
		if req.Method == "listAccounts" && h.id != nil {
			<-h.id
		}
		if ok {
			fmt.Fprintf(c, `{"jsonrpc":"2.0","result":%s,"id":%s}`+"\n", res, req.ID)
		}
	}
}

// model is the fake OpenRouter.
func (h *harness) model(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
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

// set makes signal-cli answer method with result.
func (h *harness) set(method, result string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.res[method] = result
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

// msg is a message to receive.
type msg struct {
	from, name, text string
	group, atBot     bool
	images           []string
	quote            *quoted
	expires          int
}

type quoted struct{ author, text string }

func dm(text string) msg { return msg{from: aliceID, name: "Alice", text: text} }

func atBot(text string) msg {
	return msg{from: aliceID, name: "Alice", text: "\uFFFC " + text, group: true, atBot: true}
}

func chatter(text string) msg { return msg{from: bobID, name: "Bob", text: text, group: true} }

// receive sends m to the bot as signal-cli would.
func (h *harness) receive(m msg) {
	d := map[string]any{"message": m.text, "expiresInSeconds": m.expires}
	if m.group {
		d["groupInfo"] = map[string]any{"groupId": groupID, "type": "DELIVER"}
	}
	if m.atBot {
		d["mentions"] = []any{map[string]any{"uuid": botID, "name": "Botty", "start": 0, "length": 1}}
	}
	if q := m.quote; q != nil {
		d["quote"] = map[string]any{"author": q.author, "authorUuid": q.author, "text": q.text}
	}
	var atts []any
	for _, id := range m.images {
		atts = append(atts, map[string]any{"id": id, "contentType": "image/png", "size": 100})
	}
	d["attachments"] = atts
	env := map[string]any{"source": m.from, "sourceUuid": m.from, "sourceName": m.name, "dataMessage": d}
	frame, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "receive", "params": map[string]any{"envelope": env}})
	h.mu.Lock()
	c := h.conn
	h.mu.Unlock()
	c.Write(append(frame, '\n'))
}

// nextSend is the next message the bot sends.
func (h *harness) nextSend() map[string]any {
	h.t.Helper()
	select {
	case s := <-h.sent:
		return s
	case <-time.After(5 * time.Second):
		h.t.Fatalf("nothing sent; logs:\n%s", h.logs)
		return nil
	}
}

// noSend checks that the bot sends nothing more for a moment.
func (h *harness) noSend() {
	h.t.Helper()
	select {
	case s := <-h.sent:
		h.t.Fatalf("sent %v", s)
	case <-time.After(100 * time.Millisecond):
	}
}

// ask receives m and returns the model request and the reply sent.
func (h *harness) ask(m msg) (map[string]any, map[string]any) {
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

func (h *harness) calledWith(method string) []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]any
	for _, c := range h.calls {
		if c.Method == method {
			out = append(out, c.Params)
		}
	}
	return out
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
		f, _ := t.(map[string]any)["function"].(map[string]any)
		out = append(out, fmt.Sprint(f["name"]))
	}
	return out
}

func equal(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("%s:\n got  %q\n want %q", what, got, want)
	}
}

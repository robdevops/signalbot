//go:build !notelegram

package main

// Scenarios for the bot on Telegram: the real bot, its features and the
// Telegram adapter, against a fake Bot API and a fake model API. The Signal
// scenarios cover the core's rules; these cover what Telegram changes.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

const (
	tgToken = "123456:SECRET-test-token"
	tgGroup = "-1001234567890"
	tgAlice = "1111111"
	tgBob   = "2222222"
	tgWait  = 10 * time.Second
)

// tgCall is one Bot API call the bot made: its method and parameters, from
// JSON or a multipart form, and the names of the files uploaded.
type tgCall struct {
	Method string
	Params map[string]any
	Files  []string
}

// tgHarness is a running bot on Telegram and its fakes.
type tgHarness struct {
	t       *testing.T
	mu      sync.Mutex
	updates chan string // update JSON for the next getUpdates
	nextID  int         // the next update_id, and the next message_id the bot is given
	calls   []tgCall
	replies []string  // the model's next replies; then "ok"; a reply starting "tool:" calls make_chart with the rest
	reqs    []tgModel // every chat completions request
	logs    bytes.Buffer
}

// tgModel is one chat completions request: each message's role and text,
// and whether the last carried an image.
type tgModel struct {
	Contents []string
	Image    bool
	Tools    []string
}

// startTelegram starts the bot on Telegram with its fakes, as main does, and
// returns once it has connected.
func startTelegram(t *testing.T, env map[string]string) *tgHarness {
	h := &tgHarness{t: t, updates: make(chan string, 64), nextID: 100}
	api := httptest.NewServer(http.HandlerFunc(h.serveAPI))
	t.Cleanup(api.Close)
	model := httptest.NewServer(http.HandlerFunc(h.serveModel))
	t.Cleanup(model.Close)

	for _, k := range []string{"SIGNALCLI_SOCKET", "SIGNAL_DM_USER_IDS", "MODEL_API_KEY", "MODEL_SEARCH", "MODEL_REASONING", "MODEL_SYSTEM_PROMPT",
		"INTRO", "MAX_CONCURRENCY", "HISTORY_MAX", "TOOL_LINGER_ROUNDS", "MCP_CONFIG", "GROUP_CONTEXT", "FILES", "CHARTS", "JOURNAL_STREAM", "DAILY_BUDGET"} {
		t.Setenv(k, "")
	}
	t.Setenv("TELEGRAM_DM_USER_IDS", "") // no longer read
	settings := map[string]string{"TELEGRAM_BOT_TOKEN": tgToken, "TELEGRAM_API_URL": api.URL,
		"MODEL_API_URL": model.URL, "MODEL": "test/test-model", "STATE_DIRECTORY": t.TempDir()}
	for k, v := range env {
		settings[k] = v
	}
	for k, v := range settings {
		t.Setenv(k, v)
	}
	t.Cleanup(logx.SetOutput(h, h, true))

	test := &config.Test{RetryDelay: 10 * time.Millisecond}
	cfg := config.Load()
	cfg.Test = test
	net, err := newNetwork(cfg.Test)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	b := bot.New(ctx, cfg, net)
	for _, register := range enabled() {
		if err := register(b); err != nil {
			t.Fatal(err)
		}
	}
	stopped := make(chan struct{})
	go func() { defer close(stopped); b.Run() }()
	t.Cleanup(func() {
		h.waitFor("the bot to finish its work", test.Idle)
		cancel()
		select {
		case <-stopped:
		case <-time.After(tgWait):
			t.Error("the bot did not stop")
		}
		h.waitFor("the bot to finish its work", test.Idle)
		if strings.Contains(h.logText(), "SECRET-test-token") {
			t.Errorf("the token was logged:\n%s", h.logText())
		}
	})
	h.waitFor("the bot to connect", func() bool { return strings.Contains(h.logText(), "connected to Telegram as @botty_bot") })
	return h
}

func (h *tgHarness) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.logs.Write(p)
}

func (h *tgHarness) logText() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.logs.String()
}

func (h *tgHarness) waitFor(what string, ok func() bool) {
	h.t.Helper()
	for end := time.Now().Add(tgWait); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			h.t.Fatalf("timed out waiting for %s; calls %v", what, h.methods())
		}
	}
}

// --- the Bot API -----------------------------------------------------------------

func (h *tgHarness) serveAPI(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/file/bot"+tgToken+"/") {
		w.Write(tgPNG())
		return
	}
	method, ok := strings.CutPrefix(r.URL.Path, "/bot"+tgToken+"/")
	if !ok {
		http.Error(w, `{"ok":false,"error_code":401,"description":"Unauthorized"}`, http.StatusUnauthorized)
		return
	}
	call := tgCall{Method: method, Params: map[string]any{}}
	if mt, ps, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "multipart/form-data" {
		mr := multipart.NewReader(r.Body, ps["boundary"])
		for p, err := mr.NextPart(); err == nil; p, err = mr.NextPart() {
			b, _ := io.ReadAll(p)
			if p.FileName() != "" {
				call.Files = append(call.Files, p.FormName())
			} else {
				call.Params[p.FormName()] = string(b)
			}
		}
	} else {
		json.NewDecoder(r.Body).Decode(&call.Params)
	}
	result := "true"
	switch method {
	case "getUpdates":
		select {
		case u := <-h.updates:
			result = "[" + u + "]"
		case <-time.After(100 * time.Millisecond):
			result = "[]"
		case <-r.Context().Done():
			return
		}
	case "getMe":
		result = `{"id":777000,"is_bot":true,"first_name":"Botty","username":"botty_bot"}`
	case "getChat":
		result = `{"id":1,"type":"group"}`
	case "getChatMember":
		result = `{"status":"member","user":{"id":1}}`
	case "getChatAdministrators":
		result = `[]`
	case "getFile":
		result = `{"file_id":"f","file_path":"photos/1.png","file_size":100}`
	case "sendMessage", "sendPhoto":
		h.mu.Lock()
		h.nextID++
		result = fmt.Sprintf(`{"message_id":%d}`, h.nextID)
		h.mu.Unlock()
	}
	if method != "getUpdates" {
		h.mu.Lock()
		h.calls = append(h.calls, call)
		h.mu.Unlock()
	}
	fmt.Fprintf(w, `{"ok":true,"result":%s}`, result)
}

// send delivers an update holding msg, a message's JSON without its
// message_id, and returns that ID.
func (h *tgHarness) send(kind, msg string) int {
	h.mu.Lock()
	h.nextID++
	n := h.nextID
	h.mu.Unlock()
	h.updates <- fmt.Sprintf(`{"update_id":%d,%q:{"message_id":%d,"date":%d,%s}}`, n, kind, n, time.Now().Unix(), msg)
	return n
}

const (
	tgFromAlice = `"from":{"id":1111111,"first_name":"Alice"}`
	tgFromBob   = `"from":{"id":2222222,"first_name":"Bob"}`
	tgInGroup   = `"chat":{"id":-1001234567890,"type":"supergroup","title":"Test group"}`
	tgInDM      = `"chat":{"id":1111111,"type":"private","first_name":"Alice"}`
)

func (h *tgHarness) groupText(from, text string) int {
	return h.send("message", from+","+tgInGroup+`,"text":`+jsonString(text))
}

func (h *tgHarness) atBot(text string) int {
	return h.send("message", tgFromAlice+","+tgInGroup+`,"text":`+jsonString("@botty_bot "+text)+
		`,"entities":[{"type":"mention","offset":0,"length":10}]`)
}

func jsonString(s string) string { b, _ := json.Marshal(s); return string(b) }

func (h *tgHarness) callsTo(method string) []tgCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []tgCall
	for _, c := range h.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (h *tgHarness) methods() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, c := range h.calls {
		out = append(out, c.Method)
	}
	return out
}

// waitCalls waits until method has been called n times in all, and returns
// the calls.
func (h *tgHarness) waitCalls(method string, n int) []tgCall {
	h.t.Helper()
	h.waitFor(fmt.Sprintf("%d %s", n, method), func() bool { return len(h.callsTo(method)) >= n })
	return h.callsTo(method)
}

func tgPNG() []byte {
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for i := range 8 {
		img.Set(i, i, color.RGBA{255, 0, 0, 255})
	}
	var b bytes.Buffer
	png.Encode(&b, img)
	return b.Bytes()
}

// --- the model API --------------------------------------------------------------

// chartTool is the chart a "tool:" reply asks make_chart for.
const tgChart = `{"title":"NVDA, 1 month","series":[{"name":"NVDA","points":[{"date":"2026-09-30","value":180},{"date":"2026-10-15","value":190},{"date":"2026-10-28","value":201}]}]}`

func (h *tgHarness) serveModel(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		fmt.Fprint(w, `{"data":[{"id":"test/test-model","architecture":{"input_modalities":["text","image"]},"supported_parameters":["tools"]}]}`)
		return
	}
	var body struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Function struct{ Name string } `json:"function"`
		} `json:"tools"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	var req tgModel
	for _, m := range body.Messages {
		var text string
		if json.Unmarshal(m.Content, &text) != nil {
			var parts []struct{ Type, Text string }
			json.Unmarshal(m.Content, &parts)
			req.Image = false
			for _, p := range parts {
				text += p.Text
				req.Image = req.Image || p.Type == "image_url"
			}
		} else {
			req.Image = false
		}
		if m.Role != "system" {
			req.Contents = append(req.Contents, m.Role+": "+text)
		}
	}
	for _, t := range body.Tools {
		req.Tools = append(req.Tools, t.Function.Name)
	}
	h.mu.Lock()
	h.reqs = append(h.reqs, req)
	reply := "ok"
	if len(h.replies) > 0 {
		reply, h.replies = h.replies[0], h.replies[1:]
	}
	n := len(h.reqs)
	h.mu.Unlock()
	if args, ok := strings.CutPrefix(reply, "tool:"); ok {
		a, _ := json.Marshal(args)
		fmt.Fprintf(w, `{"choices":[{"message":{"content":"","tool_calls":[{"id":"call_%d","type":"function","function":{"name":"make_chart","arguments":%s}}]}}]}`, n, a)
		return
	}
	fmt.Fprintf(w, `{"choices":[{"message":{"content":%s}}]}`, jsonString(reply))
}

func (h *tgHarness) queue(replies ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.replies = append(h.replies, replies...)
}

func (h *tgHarness) requests() []tgModel {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.reqs)
}

// --- scenarios --------------------------------------------------------------------

// In a group, the bot answers a mention, with the chatter before it as
// context, and its Markdown arrives as Telegram's entities; chatter alone
// gets no answer.
func TestTelegramGroupMention(t *testing.T) {
	h := startTelegram(t, nil)
	h.groupText(tgFromBob, "the moon is made of cheese")
	h.queue("**Yes**, see [the docs](https://example.com/d)")
	h.atBot("is it?")
	s := h.waitCalls("sendMessage", 1)[0].Params
	if s["chat_id"] != tgGroup || s["text"] != "Yes, see the docs" {
		t.Fatalf("sent %v", s)
	}
	ents, _ := json.Marshal(s["entities"])
	if string(ents) != `[{"length":3,"offset":0,"type":"bold"},{"length":8,"offset":9,"type":"text_link","url":"https://example.com/d"}]` {
		t.Fatalf("entities %s", ents)
	}
	r := h.requests()
	if len(r) != 1 || !slices.Equal(r[0].Contents, []string{"user: [Messages not addressed to you, for context only:]\nBob: the moon is made of cheese", "user: Alice: is it?"}) {
		t.Fatalf("model saw %+v", r)
	}
	if strings.Contains(h.logText(), "moon") || strings.Contains(h.logText(), "is it?") {
		t.Fatalf("message text logged:\n%s", h.logText())
	}
}

// Anyone's DM is answered, and an edit of one is answered again.
func TestTelegramDMs(t *testing.T) {
	h := startTelegram(t, nil)
	h.send("message", tgFromBob+`,"chat":{"id":2222222,"type":"private","first_name":"Bob"},"text":"let me in"`)
	if s := h.waitCalls("sendMessage", 1)[0].Params; s["chat_id"] != tgBob || s["text"] != "ok" {
		t.Fatalf("sent to Bob %v", s)
	}
	id := h.send("message", tgFromAlice+","+tgInDM+`,"text":"hello"`)
	if s := h.waitCalls("sendMessage", 2)[1].Params; s["chat_id"] != tgAlice || s["text"] != "ok" {
		t.Fatalf("sent %v", s)
	}
	h.updates <- fmt.Sprintf(`{"update_id":999,"edited_message":{"message_id":%d,"date":1,"edit_date":2,%s,%s,"text":"hello there"}}`, id, tgFromAlice, tgInDM)
	h.waitCalls("sendMessage", 3)
	r := h.requests()
	if len(r) != 3 || !strings.Contains(r[2].Contents[len(r[2].Contents)-1], "hello there") {
		t.Fatalf("model saw %+v", r)
	}
}

// A photo sent to the bot is fetched and shown to the model.
func TestTelegramPhoto(t *testing.T) {
	h := startTelegram(t, nil)
	h.send("message", tgFromAlice+","+tgInDM+`,"caption":"what is this?","photo":[{"file_id":"small","width":90,"height":90,"file_size":100}]`)
	h.waitCalls("sendMessage", 1)
	if f := h.callsTo("getFile"); len(f) != 1 || f[0].Params["file_id"] != "small" {
		t.Fatalf("getFile %v", f)
	}
	if r := h.requests(); len(r) != 1 || !r[0].Image || !strings.Contains(r[0].Contents[0], "what is this?") {
		t.Fatalf("model saw %+v", r)
	}
}

// A chart is sent as a photo captioned with its title.
func TestTelegramChart(t *testing.T) {
	if chartsFeature == nil {
		t.Skip("charts left out of this build")
	}
	h := startTelegram(t, nil)
	h.queue("tool:"+tgChart, "Done")
	h.send("message", tgFromAlice+","+tgInDM+`,"text":"chart NVDA for a month"`)
	p := h.waitCalls("sendPhoto", 1)[0]
	if p.Params["chat_id"] != tgAlice || p.Params["caption"] != "NVDA, 1 month ▲ 11.7%" || !slices.Equal(p.Files, []string{"p0"}) || p.Params["photo"] != "attach://p0" {
		t.Fatalf("sendPhoto %+v", p)
	}
	if m := h.callsTo("sendMessage"); len(m) != 0 {
		t.Fatalf("also sent %v", m)
	}
}

// A one-emoji reply is a reaction.
func TestTelegramReaction(t *testing.T) {
	h := startTelegram(t, nil)
	h.queue("👍")
	id := h.send("message", tgFromAlice+","+tgInDM+`,"text":"thanks"`)
	r := h.waitCalls("setMessageReaction", 1)[0].Params
	if r["chat_id"] != tgAlice || r["message_id"] != float64(id) {
		t.Fatalf("reaction %v", r)
	}
	if m := h.callsTo("sendMessage"); len(m) != 0 {
		t.Fatalf("also sent %v", m)
	}
}

// /intro on Telegram names the bot by its name and says how to address it.
func TestTelegramIntro(t *testing.T) {
	h := startTelegram(t, nil)
	h.atBot("/intro")
	s := h.waitCalls("sendMessage", 1)[0].Params
	if txt, _ := s["text"].(string); !strings.HasPrefix(txt, "hi, I'm Botty") {
		t.Fatalf("sent %q", txt)
	}
	if n := len(h.requests()); n != 0 {
		t.Fatalf("the model was asked %d times", n)
	}
}

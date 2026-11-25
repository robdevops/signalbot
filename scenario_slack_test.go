//go:build !noslack

package main

// Scenarios for the bot on Slack: the real bot, its features and the Slack
// adapter, against a fake Web API and Socket Mode server and a fake model
// API. The Signal scenarios cover the core's rules; these cover what Slack
// changes.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/ws/wstest"
)

const (
	slToken    = "xoxb-1-2-SECRETslackbot"
	slAppToken = "xapp-1-A1-SECRETslackapp"
	slChannel  = "C0CHANNEL1"
	slAlice    = "UALICE001"
	slBob      = "UBOB00001"
	slWait     = 10 * time.Second
)

// slHarness is a running bot on Slack and its fakes.
type slHarness struct {
	t       *testing.T
	mu      sync.Mutex
	conn    *wstest.Conn
	nextTS  int
	envs    int
	calls   []slCall
	replies []string        // the model's next replies; then "ok"
	admins  map[string]bool // users users.info says are workspace admins
	reqs    [][]string      // each chat completions request's messages, role and text
	logs    bytes.Buffer
}

// slCall is one Web API call the bot made: its method and parameters.
type slCall struct {
	Method string
	Params map[string]any
}

// startSlack starts the bot on Slack with its fakes, as main does, with
// extra features registered after the bot's own, and returns once it has
// connected.
func startSlack(t *testing.T, extra ...func(feature.Host) error) *slHarness {
	return startSlackWith(t, nil, extra...)
}

// startSlackWith is startSlack with the settings env too.
func startSlackWith(t *testing.T, env map[string]string, extra ...func(feature.Host) error) *slHarness {
	h := &slHarness{t: t, admins: map[string]bool{}}
	api := httptest.NewServer(http.HandlerFunc(h.serveAPI))
	t.Cleanup(api.Close)
	model := httptest.NewServer(http.HandlerFunc(h.serveModel))
	t.Cleanup(model.Close)
	for _, k := range []string{"SIGNALCLI_SOCKET", "TELEGRAM_BOT_TOKEN", "MODEL_API_KEY", "MODEL_SEARCH", "MODEL_REASONING", "MODEL_SYSTEM_PROMPT",
		"INTRO", "MAX_CONCURRENCY", "HISTORY_MAX", "TOOL_LINGER_ROUNDS", "MCP_CONFIG", "GROUP_CONTEXT", "FILES", "CHARTS", "JOURNAL_STREAM", "DAILY_BUDGET",
		"SLACK_DM_USERS", "SCHEDULE_ALLOWED_USERS"} {
		t.Setenv(k, "")
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	for k, v := range map[string]string{"SLACK_BOT_TOKEN": slToken, "SLACK_APP_TOKEN": slAppToken, "SLACK_API_URL": api.URL,
		"MODEL_API_URL": model.URL, "MODEL": "test/test-model", "STATE_DIRECTORY": t.TempDir()} {
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
	for _, register := range append(enabled(), extra...) {
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
		case <-time.After(slWait):
			t.Error("the bot did not stop")
		}
		h.waitFor("the bot to finish its work", test.Idle)
		if l := h.logText(); strings.Contains(l, "SECRET") {
			t.Errorf("a token was logged:\n%s", l)
		}
	})
	h.waitFor("the bot to connect", func() bool { return strings.Contains(h.logText(), "connected to Slack as @botty") })
	return h
}

func (h *slHarness) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.logs.Write(p)
}

func (h *slHarness) logText() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.logs.String()
}

func (h *slHarness) waitFor(what string, ok func() bool) {
	h.t.Helper()
	for end := time.Now().Add(slWait); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			h.t.Fatalf("timed out waiting for %s; calls %v", what, h.methods())
		}
	}
}

// --- Slack -------------------------------------------------------------------

func (h *slHarness) serveAPI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/socket" {
		conn, err := wstest.Accept(w, r)
		if err != nil {
			return
		}
		conn.Write(`{"type":"hello"}`)
		h.mu.Lock()
		h.conn = conn
		h.mu.Unlock()
		for {
			if _, err := conn.Read(); err != nil { // the acknowledgements
				return
			}
		}
	}
	method := strings.TrimPrefix(r.URL.Path, "/")
	call := slCall{Method: method, Params: map[string]any{}}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		json.NewDecoder(r.Body).Decode(&call.Params)
	} else {
		r.ParseForm()
		for k := range r.PostForm {
			call.Params[k] = r.PostForm.Get(k)
		}
	}
	h.mu.Lock()
	h.calls = append(h.calls, call)
	res := ""
	switch method {
	case "auth.test":
		res = `"user_id":"UBOT00001","user":"botty"`
	case "apps.connections.open":
		res = `"url":"ws://` + r.Host + `/socket"`
	case "users.info":
		u := r.PostForm.Get("user")
		if n := map[string]string{slAlice: "Alice", slBob: "Bob", "UBOT00001": "Botty"}[u]; n != "" {
			res = fmt.Sprintf(`"user":{"is_admin":%t,"profile":{"display_name":%q}}`, h.admins[u], n)
		}
	case "conversations.open":
		res = `"channel":{"id":"D0ALICEDM1"}`
	case "chat.postMessage":
		h.nextTS++
		res = fmt.Sprintf(`"ts":"1700000100.%06d"`, h.nextTS)
	}
	h.mu.Unlock()
	if res != "" {
		res = "," + res
	}
	fmt.Fprintf(w, `{"ok":true%s}`, res)
}

// envelope delivers a Socket Mode envelope of kind holding payload.
func (h *slHarness) envelope(kind, payload string) {
	h.waitFor("the socket", func() bool { h.mu.Lock(); defer h.mu.Unlock(); return h.conn != nil })
	h.mu.Lock()
	h.envs++
	conn, n := h.conn, h.envs
	h.mu.Unlock()
	conn.Write(fmt.Sprintf(`{"type":%q,"envelope_id":"env%d","payload":%s}`, kind, n, payload))
}

// slNames are the display names Slack sends with a person's message.
var slNames = map[string]string{slAlice: "Alice", slBob: "Bob"}

// message delivers a message event from user in channel (a DM's starts
// with D), with their profile as Slack sends it, and returns its ts.
func (h *slHarness) message(user, channel, text string) string {
	h.mu.Lock()
	h.nextTS++
	ts := fmt.Sprintf("1700000000.%06d", h.nextTS)
	h.mu.Unlock()
	kind := "channel"
	if strings.HasPrefix(channel, "D") {
		kind = "im"
	}
	h.envelope("events_api", fmt.Sprintf(`{"event":{"type":"message","channel":%q,"channel_type":%q,"user":%q,"text":%s,"ts":%q,"user_profile":{"display_name":%q}}}`,
		channel, kind, user, jsonStr(text), ts, slNames[user]))
	return ts
}

func jsonStr(s string) string { b, _ := json.Marshal(s); return string(b) }

func (h *slHarness) callsTo(method string) []slCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []slCall
	for _, c := range h.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (h *slHarness) methods() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, c := range h.calls {
		out = append(out, c.Method)
	}
	return out
}

func (h *slHarness) waitCalls(method string, n int) []slCall {
	h.t.Helper()
	h.waitFor(fmt.Sprintf("%d %s", n, method), func() bool { return len(h.callsTo(method)) >= n })
	return h.callsTo(method)
}

// sectionText is a posted message's first section's mrkdwn.
func sectionText(c slCall) string {
	blocks, _ := c.Params["blocks"].([]any)
	for _, b := range blocks {
		if m, _ := b.(map[string]any); m["type"] == "section" {
			t, _ := m["text"].(map[string]any)["text"].(string)
			return t
		}
	}
	return ""
}

// --- the model API ------------------------------------------------------------

func (h *slHarness) serveModel(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		fmt.Fprint(w, `{"data":[{"id":"test/test-model","architecture":{"input_modalities":["text","image"]},"supported_parameters":["tools"]}]}`)
		return
	}
	var body struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	json.NewDecoder(r.Body).Decode(&body)
	var req []string
	for _, m := range body.Messages {
		var text string
		if json.Unmarshal(m.Content, &text) != nil {
			var parts []struct{ Text string }
			json.Unmarshal(m.Content, &parts)
			for _, p := range parts {
				text += p.Text
			}
		}
		if m.Role != "system" {
			req = append(req, m.Role+": "+text)
		}
	}
	h.mu.Lock()
	h.reqs = append(h.reqs, req)
	reply := "ok"
	if len(h.replies) > 0 {
		reply, h.replies = h.replies[0], h.replies[1:]
	}
	h.mu.Unlock()
	fmt.Fprintf(w, `{"choices":[{"message":{"content":%s}}]}`, jsonStr(reply))
}

func (h *slHarness) queue(replies ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.replies = append(h.replies, replies...)
}

func (h *slHarness) requests() [][]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.reqs)
}

// --- scenarios --------------------------------------------------------------------

// A DM from someone SLACK_DM_USERS lists is answered, in the DM.
func TestSlackDM(t *testing.T) {
	h := startSlackWith(t, map[string]string{"SLACK_DM_USERS": slAlice})
	h.message(slAlice, "D0ALICEDM1", "hello")
	s := h.waitCalls("chat.postMessage", 1)[0]
	if s.Params["channel"] != "D0ALICEDM1" || s.Params["text"] != "ok" || sectionText(s) != "ok" {
		t.Fatalf("sent %v", s.Params)
	}
}

// setAdmin says whether users.info has user as a workspace admin.
func (h *slHarness) setAdmin(user string, admin bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.admins[user] = admin
}

// waitLog waits for a log line holding s.
func (h *slHarness) waitLog(s string) {
	h.t.Helper()
	h.waitFor(fmt.Sprintf("the log line %q", s), func() bool { return strings.Contains(h.logText(), s) })
}

// By default a workspace admin's DM is answered and a stranger's isn't:
// not answered or kept, and logged by their ID alone.
func TestSlackDMDefault(t *testing.T) {
	h := startSlack(t)
	h.waitLog("Slack DMs: allowed from admins and mods (SLACK_DM_USERS)")
	h.setAdmin(slAlice, true)
	h.message(slBob, "D0BOBDM001", "a secret question")
	h.waitLog("slack:" + slBob + " ignored because not allowed by SLACK_DM_USERS")
	h.message(slAlice, "D0ALICEDM1", "hello")
	if s := h.waitCalls("chat.postMessage", 1); len(s) != 1 || s[0].Params["channel"] != "D0ALICEDM1" {
		t.Fatalf("sent %v", s)
	}
	if r := h.requests(); len(r) != 1 || !slices.Equal(r[0], []string{"user: hello"}) {
		t.Fatalf("model saw %q", r)
	}
	if l := h.logText(); strings.Contains(l, "secret") || strings.Contains(l, "Bob") {
		t.Fatalf("logged:\n%s", l)
	}
}

// With SLACK_DM_USERS=none no one's DM is answered, an admin's either.
func TestSlackDMNone(t *testing.T) {
	h := startSlackWith(t, map[string]string{"SLACK_DM_USERS": "none"})
	h.waitLog("Slack DMs are off: SLACK_DM_USERS is none")
	h.setAdmin(slAlice, true)
	h.message(slAlice, "D0ALICEDM1", "hello")
	h.waitLog("slack:" + slAlice + " ignored because not allowed by SLACK_DM_USERS")
	h.message(slAlice, slChannel, "<@UBOT00001> hello")
	if s := h.waitCalls("chat.postMessage", 1); len(s) != 1 || s[0].Params["channel"] != slChannel {
		t.Fatalf("sent %v", s)
	}
}

// A schedule in an admin's DM is dropped at its time once they are no
// longer an admin, so may no longer DM the bot, naming no one.
func TestSlackDMScheduleDropped(t *testing.T) {
	h := startSlack(t)
	h.setAdmin(slAlice, true)
	at := time.Now().Truncate(time.Second).Add(2 * time.Second)
	h.message(slAlice, "D0ALICEDM1", `/schedule "lunch is ready" `+at.Format("2006-01-02T15:04:05"))
	s := h.waitCalls("chat.postMessage", 1)[0]
	if txt, _ := s.Params["text"].(string); !strings.HasPrefix(txt, "Scheduled for ") {
		t.Fatalf("sent %v", s.Params)
	}
	h.setAdmin(slAlice, false)
	h.waitLog("delivered message dropped: its sender may no longer DM the bot (SLACK_DM_USERS)")
	if n := len(h.callsTo("chat.postMessage")); n != 1 {
		t.Fatalf("%d sends", n)
	}
	if l := h.logText(); strings.Contains(l, "lunch") || strings.Contains(l, slAlice) {
		t.Fatalf("logged:\n%s", l)
	}
}

// In a channel, the bot answers a mention, with the chatter before it as
// context, and its Markdown arrives as Slack's mrkdwn; chatter alone gets
// no answer, and no message text is logged.
func TestSlackMention(t *testing.T) {
	h := startSlack(t)
	h.message(slBob, slChannel, "the moon is made of cheese")
	h.queue("**Yes**, see [the docs](https://example.com/d)")
	h.message(slAlice, slChannel, "<@UBOT00001> is it?")
	s := h.waitCalls("chat.postMessage", 1)[0]
	if s.Params["channel"] != slChannel || sectionText(s) != "*Yes*, see <https://example.com/d|the docs>" {
		t.Fatalf("sent %v", s.Params)
	}
	r := h.requests()
	if len(r) != 1 || !slices.Equal(r[0], []string{"user: [Messages not addressed to you, for context only:]\nBob: the moon is made of cheese", "user: Alice: is it?"}) {
		t.Fatalf("model saw %q", r)
	}
	if l := h.logText(); strings.Contains(l, "moon") || strings.Contains(l, "is it?") {
		t.Fatalf("message text logged:\n%s", l)
	}
}

// A slash command is answered as a command: /intro by the bot's name, with
// no model; and the app manifest lists it.
func TestSlackSlashCommand(t *testing.T) {
	h := startSlack(t)
	h.envelope("slash_commands", `{"command":"/intro","text":"","channel_id":"C0CHANNEL1","user_id":"UALICE001"}`)
	s := h.waitCalls("chat.postMessage", 1)[0]
	if txt, _ := s.Params["text"].(string); s.Params["channel"] != slChannel || !strings.HasPrefix(txt, "hi, I'm _Botty_") {
		t.Fatalf("sent %v", s.Params)
	}
	if n := len(h.requests()); n != 0 {
		t.Fatalf("the model was asked %d times", n)
	}
	if !strings.Contains(h.logText(), "the commands changed: paste") {
		t.Fatalf("no manifest notice:\n%s", h.logText())
	}
}

// A button press reaches a feature's OnPress hook, which the envelope's
// acknowledgement already answered to Slack, and the hook's answer is shown
// to the presser alone.
func TestSlackPress(t *testing.T) {
	pressed := make(chan feature.Press, 1)
	h := startSlack(t, func(host feature.Host) error {
		host.OnPress(func(_ context.Context, p feature.Press) (string, bool) {
			pressed <- p
			return "Done", true
		})
		return nil
	})
	h.envelope("interactive", `{"type":"block_actions","user":{"id":"UALICE001","name":"alice"},"channel":{"id":"C0CHANNEL1"},
		"container":{"type":"message","message_ts":"1700000100.000001"},"message":{"ts":"1700000100.000001"},"actions":[{"action_id":"b0_0","value":"x:on"}]}`)
	select {
	case p := <-pressed:
		if p.Chat.GroupID != "slack:"+slChannel || p.Data != "x:on" || p.Sent != 1700000100000001 || p.By.ID != "slack:"+slAlice {
			t.Fatalf("press %+v", p)
		}
	case <-time.After(slWait):
		t.Fatal("the press never reached the feature")
	}
	e := h.waitCalls("chat.postEphemeral", 1)[0].Params
	if e["user"] != slAlice || e["channel"] != slChannel || e["text"] != "Done" {
		t.Fatalf("answer %v", e)
	}
}

//go:build !nodiscord

package main

// Scenarios for the bot on Discord: the real bot, its features and the
// Discord adapter, against a fake Discord (REST API and gateway) and a fake
// model API. The Signal scenarios cover the core's rules; these cover what
// Discord changes.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	dcToken   = "MTIz.SECRET-test-token"
	dcBot     = "900000000000000001"
	dcAlice   = "100000000000000001"
	dcBob     = "100000000000000002"
	dcGuild   = "300000000000000001"
	dcChannel = "400000000000000001"
	dcDM      = "500000000000000001"
	dcWait    = 10 * time.Second
)

// dcCall is one REST request the bot made: its method and path, and its
// JSON body, or a form's payload_json.
type dcCall struct {
	Route string
	Body  map[string]any
	Raw   string // the JSON as sent
}

// dcHarness is a running bot on Discord and its fakes.
type dcHarness struct {
	t       *testing.T
	mu      sync.Mutex
	events  chan string // gateway events to dispatch, as {"t":…,"d":…}
	nextID  int64       // the next message ID
	seq     int
	calls   []dcCall
	replies []string // the model's next replies; then "ok"
	reqs    [][]string
	logs    strings.Builder
	presses []feature.Press   // what the test feature's OnPress saw
	members map[string]string // a server member's JSON, by user ID; anyone else is no member
}

// startDiscord starts the bot on Discord with its fakes, as main does, and
// env's settings, and a test feature with /buttons, whose buttons' presses
// it records, and returns once the bot has connected. DISCORD_DM_USER_IDS
// is unset unless env sets it.
func startDiscord(t *testing.T, env map[string]string) *dcHarness {
	h := &dcHarness{t: t, events: make(chan string, 64), nextID: 1300000000000000000, members: map[string]string{}}
	api := httptest.NewServer(http.HandlerFunc(h.serveAPI))
	t.Cleanup(api.Close)
	model := httptest.NewServer(http.HandlerFunc(h.serveModel))
	t.Cleanup(model.Close)
	for _, k := range []string{"SIGNALCLI_SOCKET", "SIGNAL_DM_USER_IDS", "TELEGRAM_BOT_TOKEN", "MODEL_API_KEY", "MODEL_SEARCH", "MODEL_REASONING", "MODEL_SYSTEM_PROMPT",
		"INTRO", "MAX_CONCURRENCY", "HISTORY_MAX", "TOOL_LINGER_ROUNDS", "MCP_CONFIG", "GROUP_CONTEXT", "FILES", "CHARTS", "JOURNAL_STREAM", "DAILY_BUDGET", "DISCORD_DM_USER_IDS"} {
		t.Setenv(k, "")
	}
	for k, v := range env {
		t.Setenv(k, v)
	}
	for k, v := range map[string]string{"DISCORD_BOT_TOKEN": dcToken, "DISCORD_API_URL": api.URL,
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
	buttons := func(fh feature.Host) error {
		fh.OnPress(func(_ context.Context, p feature.Press) (string, bool) {
			h.mu.Lock()
			h.presses = append(h.presses, p)
			h.mu.Unlock()
			return "pressed " + p.Data, true
		})
		return fh.Command("buttons", func(context.Context, feature.Message, string) feature.Action {
			return feature.Action{Answer: "pick one", Markup: feature.Markup{Buttons: [][]feature.Button{{{Label: "A", Data: "a", Highlight: true}, {Label: "B", Data: "b"}}}}}
		})
	}
	for _, register := range append(enabled(), buttons) {
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
		case <-time.After(dcWait):
			t.Error("the bot did not stop")
		}
		if strings.Contains(h.logText(), "SECRET-test-token") {
			t.Errorf("the token was logged:\n%s", h.logText())
		}
	})
	h.waitFor("the bot to connect", func() bool { return strings.Contains(h.logText(), "connected to Discord as botty") })
	return h
}

func (h *dcHarness) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.logs.Write(p)
}

func (h *dcHarness) logText() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.logs.String()
}

func (h *dcHarness) waitFor(what string, ok func() bool) {
	h.t.Helper()
	for end := time.Now().Add(dcWait); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			h.t.Fatalf("timed out waiting for %s; calls %v", what, h.routes())
		}
	}
}

// --- Discord ----------------------------------------------------------------------

func (h *dcHarness) serveAPI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/gw" {
		h.gateway(w, r)
		return
	}
	hook := strings.HasPrefix(r.URL.Path, "/webhooks/") || strings.HasPrefix(r.URL.Path, "/interactions/")
	if !hook && r.Header.Get("Authorization") != "Bot "+dcToken {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"401: Unauthorized","code":0}`))
		return
	}
	call := dcCall{Route: r.Method + " " + r.URL.Path, Body: map[string]any{}}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		r.ParseMultipartForm(1 << 20)
		json.Unmarshal([]byte(r.FormValue("payload_json")), &call.Body)
	} else {
		data, _ := io.ReadAll(r.Body)
		call.Raw = string(data)
		json.Unmarshal(data, &call.Body)
	}
	h.mu.Lock()
	h.calls = append(h.calls, call)
	h.nextID++
	n := h.nextID
	h.mu.Unlock()
	switch {
	case call.Route == "GET /gateway/bot":
		fmt.Fprintf(w, `{"url":"ws://%s/gw"}`, r.Host)
	case call.Route == "GET /applications/@me":
		fmt.Fprint(w, `{"id":"app1","bot_public":false}`)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/guilds/"+dcGuild+"/members/"):
		h.mu.Lock()
		m, ok := h.members[strings.TrimPrefix(r.URL.Path, "/guilds/"+dcGuild+"/members/")]
		h.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, `{"message":"Unknown Member","code":10007}`)
			return
		}
		fmt.Fprint(w, m)
	case call.Route == "POST /users/@me/channels":
		fmt.Fprintf(w, `{"id":%q}`, dcDM)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages"), strings.HasSuffix(r.URL.Path, "/@original"):
		fmt.Fprintf(w, `{"id":"%d"}`, n)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// gateway says hello, answers heartbeats, sends READY after IDENTIFY, then
// dispatches the test's events.
func (h *dcHarness) gateway(w http.ResponseWriter, r *http.Request) {
	conn, err := wstest.Accept(w, r)
	if err != nil {
		return
	}
	conn.Write(`{"op":10,"d":{"heartbeat_interval":45000}}`)
	identified := make(chan struct{})
	go func() {
		for {
			s, err := conn.Read()
			if err != nil {
				return
			}
			switch {
			case strings.HasPrefix(s, `{"d":null,"op":1}`), strings.Contains(s, `"op":1}`):
				conn.Write(`{"op":11}`)
			case strings.Contains(s, `"op":2`):
				close(identified)
			}
		}
	}()
	select {
	case <-identified:
	case <-time.After(dcWait):
		return
	}
	h.dispatch(conn, "READY", `{"v":10,"session_id":"s1","resume_gateway_url":"ws://`+r.Host+`/gw","user":{"id":"`+dcBot+`","username":"botty","global_name":"Botty","bot":true},"application":{"id":"app1"},"guilds":[{"id":"`+dcGuild+`","unavailable":true}]}`)
	h.dispatch(conn, "GUILD_CREATE", `{"id":"`+dcGuild+`","name":"Test server","channels":[{"id":"`+dcChannel+`","name":"general","type":0}],"threads":[]}`)
	for {
		select {
		case e := <-h.events:
			var ev struct {
				T string          `json:"t"`
				D json.RawMessage `json:"d"`
			}
			json.Unmarshal([]byte(e), &ev)
			h.dispatch(conn, ev.T, string(ev.D))
		case <-r.Context().Done():
			return
		case <-time.After(dcWait):
			return
		}
	}
}

func (h *dcHarness) dispatch(conn *wstest.Conn, t, d string) {
	h.mu.Lock()
	h.seq++
	s := h.seq
	h.mu.Unlock()
	conn.Write(fmt.Sprintf(`{"op":0,"s":%d,"t":%q,"d":%s}`, s, t, d))
}

// send dispatches an event, and returns the message ID it is given, if it
// is a message.
func (h *dcHarness) send(t, d string) string {
	h.mu.Lock()
	h.nextID++
	n := h.nextID
	h.mu.Unlock()
	id := fmt.Sprint(n)
	h.events <- fmt.Sprintf(`{"t":%q,"d":%s}`, t, strings.ReplaceAll(d, "$ID", id))
	return id
}

const (
	dcFromAlice = `"author":{"id":"` + dcAlice + `","username":"alice","global_name":"Alice"}`
	dcFromBob   = `"author":{"id":"` + dcBob + `","username":"bob","global_name":"Bob"}`
	dcInChannel = `"guild_id":"` + dcGuild + `","channel_id":"` + dcChannel + `"`
	dcInDM      = `"channel_id":"` + dcDM + `"`
)

func dcJSON(s string) string { b, _ := json.Marshal(s); return string(b) }

func (h *dcHarness) routes() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, c := range h.calls {
		out = append(out, c.Route)
	}
	return out
}

// waitCalls waits until route has been called n times in all, and returns
// the calls.
func (h *dcHarness) waitCalls(route string, n int) []dcCall {
	h.t.Helper()
	var out []dcCall
	h.waitFor(fmt.Sprintf("%d %s", n, route), func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		out = nil
		for _, c := range h.calls {
			if c.Route == route {
				out = append(out, c)
			}
		}
		return len(out) >= n
	})
	return out
}

// --- the model API --------------------------------------------------------------

func (h *dcHarness) serveModel(w http.ResponseWriter, r *http.Request) {
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
	var contents []string
	for _, m := range body.Messages {
		var text string
		if json.Unmarshal(m.Content, &text) != nil {
			var parts []struct{ Type, Text string }
			json.Unmarshal(m.Content, &parts)
			for _, p := range parts {
				text += p.Text
			}
		}
		if m.Role != "system" {
			contents = append(contents, m.Role+": "+text)
		}
	}
	h.mu.Lock()
	h.reqs = append(h.reqs, contents)
	reply := "ok"
	if len(h.replies) > 0 {
		reply, h.replies = h.replies[0], h.replies[1:]
	}
	h.mu.Unlock()
	fmt.Fprintf(w, `{"choices":[{"message":{"content":%s}}]}`, dcJSON(reply))
}

func (h *dcHarness) requests() [][]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.reqs)
}

// --- scenarios --------------------------------------------------------------------

// A DM from someone DISCORD_DM_USER_IDS lists is answered in the DM's
// channel, and the bot's commands became Discord's slash commands.
func TestDiscordDM(t *testing.T) {
	h := startDiscord(t, map[string]string{"DISCORD_DM_USER_IDS": dcBob})
	h.send("MESSAGE_CREATE", `{"id":"$ID","type":0,`+dcInDM+`,`+dcFromBob+`,"content":"hello"}`)
	if s := h.waitCalls("POST /channels/"+dcDM+"/messages", 1)[0].Body; s["content"] != "ok" {
		t.Fatalf("sent %v", s)
	}
	cmds := h.waitCalls("PUT /applications/app1/commands", 1)[0].Raw
	if !strings.Contains(cmds, `"name":"intro"`) || !strings.Contains(cmds, `"name":"buttons"`) {
		t.Fatalf("commands %s", cmds)
	}
}

// In a channel, the bot answers a mention, with the chatter before it as
// context, in Discord's Markdown; chatter alone gets no answer.
func TestDiscordMention(t *testing.T) {
	h := startDiscord(t, nil)
	h.send("MESSAGE_CREATE", `{"id":"$ID","type":0,`+dcInChannel+`,`+dcFromBob+`,"content":"the moon is made of cheese"}`)
	h.mu.Lock()
	h.replies = append(h.replies, "**Yes**, see [the docs](https://example.com/d)")
	h.mu.Unlock()
	h.send("MESSAGE_CREATE", `{"id":"$ID","type":0,`+dcInChannel+`,`+dcFromAlice+`,"content":"<@`+dcBot+`> is it?","mentions":[{"id":"`+dcBot+`","username":"botty"}]}`)
	s := h.waitCalls("POST /channels/"+dcChannel+"/messages", 1)[0].Body
	if s["content"] != "**Yes**, see [the docs](https://example.com/d)" {
		t.Fatalf("sent %v", s)
	}
	r := h.requests()
	if len(r) != 1 || !slices.Equal(r[0], []string{"user: [Messages not addressed to you, for context only:]\nBob: the moon is made of cheese", "user: Alice: is it?"}) {
		t.Fatalf("model saw %+v", r)
	}
	if strings.Contains(h.logText(), "moon") || strings.Contains(h.logText(), "is it?") {
		t.Fatalf("message text logged:\n%s", h.logText())
	}
}

// A slash command is answered in place of Discord's "thinking…", without
// the model.
func TestDiscordSlashCommand(t *testing.T) {
	h := startDiscord(t, nil)
	h.send("INTERACTION_CREATE", `{"id":"1250000000000000001","application_id":"app1","type":2,"token":"tok1",`+dcInChannel+
		`,"member":{"user":{"id":"`+dcAlice+`","username":"alice","global_name":"Alice"}},"data":{"name":"intro","type":1}}`)
	if a := h.waitCalls("POST /interactions/1250000000000000001/tok1/callback", 1)[0].Body; a["type"] != float64(5) {
		t.Fatalf("ack %v", a)
	}
	s := h.waitCalls("PATCH /webhooks/app1/tok1/messages/@original", 1)[0].Body
	if txt, _ := s["content"].(string); !strings.HasPrefix(txt, "hi, I'm *Botty*") {
		t.Fatalf("sent %q", txt)
	}
	if n := len(h.requests()); n != 0 {
		t.Fatalf("the model was asked %d times", n)
	}
}

// A feature's buttons are Discord's, and a press reaches the feature, is
// acknowledged at once, and its answer is shown only to the presser.
func TestDiscordButtons(t *testing.T) {
	h := startDiscord(t, nil)
	h.send("MESSAGE_CREATE", `{"id":"$ID","type":0,`+dcInChannel+`,`+dcFromAlice+`,"content":"<@`+dcBot+`> /buttons","mentions":[{"id":"`+dcBot+`","username":"botty"}]}`)
	s := h.waitCalls("POST /channels/"+dcChannel+"/messages", 1)[0].Body
	if c, _ := json.Marshal(s["components"]); s["content"] != "pick one" || !strings.Contains(string(c), `"custom_id":"a","label":"A","style":1`) {
		t.Fatalf("sent %v", s)
	}
	h.send("INTERACTION_CREATE", `{"id":"1250000000000000002","application_id":"app1","type":3,"token":"tok2",`+dcInChannel+
		`,"member":{"user":{"id":"`+dcBob+`","username":"bob","global_name":"Bob"}},"message":{"id":"1300000000000000099"},"data":{"custom_id":"b","component_type":2}}`)
	if a := h.waitCalls("POST /interactions/1250000000000000002/tok2/callback", 1)[0].Body; a["type"] != float64(6) {
		t.Fatalf("ack %v", a)
	}
	if a := h.waitCalls("POST /webhooks/app1/tok2", 1)[0].Body; a["content"] != "pressed b" || a["flags"] != float64(64) {
		t.Fatalf("answer %v", a)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.presses) != 1 || h.presses[0].Chat.GroupID != "discord:"+dcChannel || h.presses[0].By.ID != "discord:"+dcBob || h.presses[0].Sent != 1300000000000000099 {
		t.Fatalf("presses %+v", h.presses)
	}
}

// dcSoon is a time a schedule can be set for: the next whole second, at
// least 400ms away.
func dcSoon() time.Time {
	now := time.Now()
	at := now.Truncate(time.Second).Add(time.Second)
	if at.Sub(now) < 400*time.Millisecond {
		at = at.Add(time.Second)
	}
	return at
}

// dmSent is what the bot has posted in the DM's channel.
func (h *dcHarness) dmSent() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, c := range h.calls {
		if c.Route == "POST /channels/"+dcDM+"/messages" {
			s, _ := c.Body["content"].(string)
			out = append(out, s)
		}
	}
	return out
}

// By default only a server's admins and mods may DM the bot: a stranger's
// DM is refused, logged by their ID alone, while the owner's, and then a
// mod's, by a role with Manage Messages, are answered.
func TestDiscordDMAdminsAndMods(t *testing.T) {
	h := startDiscord(t, nil)
	if !strings.Contains(h.logText(), "Discord DMs: allowed from admins and mods (DISCORD_DM_USER_IDS)") {
		t.Fatalf("log %s", h.logText())
	}
	h.mu.Lock()
	h.members[dcAlice] = `{"roles":[]}`
	h.members[dcBob] = `{"roles":["600000000000000001"]}` // a role the server doesn't have yet
	h.mu.Unlock()
	h.send("MESSAGE_CREATE", `{"id":"$ID","type":0,`+dcInDM+`,`+dcFromBob+`,"content":"hello there"}`)
	h.waitFor("the refusal", func() bool {
		return strings.Contains(h.logText(), "discord:"+dcBob+" ignored because not allowed by DISCORD_DM_USER_IDS")
	})
	h.send("GUILD_UPDATE", `{"id":"`+dcGuild+`","name":"Test server","owner_id":"`+dcAlice+`"}`)
	h.send("MESSAGE_CREATE", `{"id":"$ID","type":0,`+dcInDM+`,`+dcFromAlice+`,"content":"hello"}`)
	h.waitFor("the owner's answer", func() bool { return len(h.dmSent()) == 1 })
	h.send("GUILD_ROLE_CREATE", `{"guild_id":"`+dcGuild+`","role":{"id":"600000000000000001","permissions":"8192"}}`)
	h.send("MESSAGE_CREATE", `{"id":"$ID","type":0,`+dcInDM+`,`+dcFromBob+`,"content":"hello again"}`)
	h.waitFor("the mod's answer", func() bool { return len(h.dmSent()) == 2 })
	if n := len(h.requests()); n != 2 {
		t.Fatalf("the model was asked %d times", n)
	}
	if strings.Contains(h.logText(), "hello") {
		t.Fatalf("message text logged:\n%s", h.logText())
	}
}

// DISCORD_DM_USER_IDS=none refuses everyone's DM, even the owner's, and
// says so at startup.
func TestDiscordDMNone(t *testing.T) {
	h := startDiscord(t, map[string]string{"DISCORD_DM_USER_IDS": "none"})
	if !strings.Contains(h.logText(), "Discord DMs are off") {
		t.Fatalf("log %s", h.logText())
	}
	h.send("GUILD_UPDATE", `{"id":"`+dcGuild+`","name":"Test server","owner_id":"`+dcAlice+`"}`)
	h.send("MESSAGE_CREATE", `{"id":"$ID","type":0,`+dcInDM+`,`+dcFromAlice+`,"content":"hello"}`)
	h.waitFor("the refusal", func() bool {
		return strings.Contains(h.logText(), "discord:"+dcAlice+" ignored because not allowed by DISCORD_DM_USER_IDS")
	})
	if s := h.dmSent(); len(s) != 0 || len(h.requests()) != 0 {
		t.Fatalf("sent %q", s)
	}
}

// A schedule in a DM is dropped once its user may no longer DM the bot,
// and the log names no one.
func TestDiscordDMScheduleDropped(t *testing.T) {
	h := startDiscord(t, nil)
	h.mu.Lock()
	h.members[dcAlice] = `{"roles":[]}`
	h.mu.Unlock()
	h.send("GUILD_UPDATE", `{"id":"`+dcGuild+`","name":"Test server","owner_id":"`+dcAlice+`"}`)
	at := dcSoon()
	h.send("MESSAGE_CREATE", `{"id":"$ID","type":0,`+dcInDM+`,`+dcFromAlice+`,"content":`+dcJSON(`/schedule "lunch is ready" `+at.Format("2006-01-02T15:04:05"))+`}`)
	h.waitFor("the schedule", func() bool { return len(h.dmSent()) == 1 })
	if s := h.dmSent()[0]; !strings.HasPrefix(s, "Scheduled for ") {
		t.Fatalf("sent %q", s)
	}
	h.send("GUILD_UPDATE", `{"id":"`+dcGuild+`","name":"Test server","owner_id":"700000000000000001"}`)
	h.waitFor("the run dropped", func() bool {
		return strings.Contains(h.logText(), "delivered message dropped: its sender may no longer DM the bot")
	})
	if s := h.dmSent(); len(s) != 1 {
		t.Fatalf("sent %q", s)
	}
	if time.Now().Before(at) {
		t.Fatalf("dropped before %v", at)
	}
	for _, l := range strings.Split(h.logText(), "\n") {
		if strings.Contains(l, "dropped") && strings.Contains(l, dcAlice) {
			t.Fatalf("log names her: %q", l)
		}
	}
}

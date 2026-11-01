package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/feature/featuretest"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

func writeConfig(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// register registers the feature with config as MCP_CONFIG, on a host whose
// model can call tools, and returns the host and the log.
func register(t *testing.T, config string, before ...func(*featuretest.Host)) (*featuretest.Host, *logx.Buffer, error) {
	t.Helper()
	var log logx.Buffer
	t.Cleanup(logx.SetOutput(&log, &log, true))
	h := featuretest.New()
	h.Bot.Model, h.Bot.CallsTools = "x/test-model", true
	h.Settings["MCP_CONFIG"] = writeConfig(t, config)
	for _, fn := range before {
		fn(h)
	}
	t.Cleanup(h.Stop)
	return h, &log, Register(h)
}

// tools is the tools offered, by the model's name.
func tools(h *featuretest.Host) map[string]feature.Tool {
	out := map[string]feature.Tool{}
	for _, g := range h.ToolGroups() {
		for _, t := range g.Tools {
			out[t.Name] = t
		}
	}
	return out
}

func names(h *featuretest.Host) string {
	var out []string
	for _, g := range h.ToolGroups() {
		out = append(out, g.Names...)
	}
	return strings.Join(out, ",")
}

func tinyPNG() string {
	var buf bytes.Buffer
	png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1)))
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

// fakeMCP is a Streamable HTTP MCP server with three tools: echo returns its
// arguments, fail reports an error, and pic returns an image.
type fakeMCP struct {
	sse          bool
	instructions string

	mu       sync.Mutex
	session  string // the live session id
	version  string // the bot's, as initialize sent it
	inits    int
	badCalls int // requests that carried the wrong session id
}

func (f *fakeMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     *int64          `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.Method == "initialize" {
		var p struct{ ClientInfo struct{ Version string } }
		json.Unmarshal(req.Params, &p)
		f.inits, f.version = f.inits+1, p.ClientInfo.Version
		f.session = fmt.Sprintf("s%d", f.inits)
		w.Header().Set("Mcp-Session-Id", f.session)
	} else if r.Header.Get("Mcp-Session-Id") != f.session {
		f.badCalls++
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	} else if r.Header.Get("MCP-Protocol-Version") != protocol {
		http.Error(w, "no protocol version", http.StatusBadRequest)
		return
	}
	if req.ID == nil { // a notification
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result string
	switch req.Method {
	case "initialize":
		ins, _ := json.Marshal(f.instructions)
		result = `{"protocolVersion":"` + protocol + `","capabilities":{"tools":{}},"serverInfo":{"name":"fake","version":"1"},"instructions":` + string(ins) + `}`
	case "tools/list":
		var p struct{ Cursor string }
		json.Unmarshal(req.Params, &p)
		if p.Cursor == "" {
			result = `{"tools":[{"name":"echo","description":"echoes","inputSchema":{"type":"object"}}],"nextCursor":"2"}`
		} else {
			result = `{"tools":[{"name":"fail","inputSchema":{"type":"object"}},{"name":"pic"}]}`
		}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		json.Unmarshal(req.Params, &p)
		args, _ := json.Marshal(p.Arguments)
		text, _ := json.Marshal("echo " + string(args))
		switch p.Name {
		case "fail":
			result = `{"content":[{"type":"text","text":"it broke"}],"isError":true}`
		case "pic":
			result = `{"content":[{"type":"image","data":"x"},{"type":"image","mimeType":"image/png","data":"` + tinyPNG() + `"},{"type":"text","text":"caption"}]}`
		default:
			result = `{"content":[{"type":"text","text":` + string(text) + `}]}`
		}
	}
	msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, *req.ID, result)
	if f.sse {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, msg)
}

func serve(t *testing.T, f *fakeMCP) string {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestNoConfig(t *testing.T) {
	h := featuretest.New()
	if err := Register(h); err != nil || len(h.ToolGroups()) != 0 {
		t.Fatalf("groups %+v, %v", h.ToolGroups(), err)
	}
	if r := h.EnvRead(); !slices.Equal(r, []string{"MCP_CONFIG"}) {
		t.Fatalf("read %v", r)
	}
}

// With no model, no server is started, and the log says so.
func TestNoModel(t *testing.T) {
	var started atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { started.Store(true) }))
	t.Cleanup(srv.Close)
	h, log, err := register(t, `{"mcpServers":{"w":{"url":"`+srv.URL+`"}}}`, func(h *featuretest.Host) { h.Bot = feature.Info{} })
	if err != nil || len(h.ToolGroups()) != 0 || started.Load() {
		t.Fatalf("groups %+v, %v, started %v", h.ToolGroups(), err, started.Load())
	}
	if l := log.String(); l != "<4>MCP off: no model\n" {
		t.Fatalf("log %q", l)
	}
}

func TestLoad(t *testing.T) {
	good := `{"mcpServers":{"w":{"url":"https://x/mcp","headers":{"A":"b"},"allowed_tools":["t"],"instructions":"p","keywords":["k"]},"l":{"command":"x","args":["y"],"blocked_tools":["u"]}}}`
	s, err := load(writeConfig(t, good))
	if err != nil || s["w"].URL != "https://x/mcp" || s["l"].Command != "x" || s["w"].Headers["A"] != "b" ||
		len(s["w"].AllowedTools) != 1 || s["w"].BlockedTools != nil || s["l"].AllowedTools != nil || len(s["l"].BlockedTools) != 1 ||
		s["w"].Instructions == nil || *s["w"].Instructions != "p" || !slices.Equal(s["w"].Keywords, []string{"k"}) {
		t.Fatalf("got %+v, %v", s, err)
	}
	for name, bad := range map[string]string{
		"both":     `{"mcpServers":{"w":{"url":"u","command":"c"}}}`,
		"neither":  `{"mcpServers":{"w":{"allowed_tools":["t"]}}}`,
		"not json": `{`,
	} {
		if _, err := load(writeConfig(t, bad)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	// A disabled server is dropped unchecked, even if it is incomplete.
	s, err = load(writeConfig(t, `{"mcpServers":{"on":{"url":"u"},"off":{"disabled":true},"off2":{"url":"u","disabled":true}}}`))
	if err != nil || len(s) != 1 || s["on"].URL != "u" {
		t.Fatalf("disabled servers: %+v, %v", s, err)
	}
	if s, err := load(writeConfig(t, `{"mcpServers":{"on":{"url":"u","disabled":false}}}`)); err != nil || len(s) != 1 {
		t.Fatalf("disabled false: %+v, %v", s, err)
	}
	if _, err := load(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing file must be an error")
	}
	// A bad config stops the bot from starting.
	if _, _, err := register(t, `{"mcpServers":{"w":{"url":"u","command":"c"}}}`); err == nil {
		t.Error("Register must refuse a bad config")
	}
}

func TestModelName(t *testing.T) {
	if got := modelName("my.server", "get forecast"); got != "my_server__get_forecast" {
		t.Errorf("got %q", got)
	}
	if got := modelName(strings.Repeat("s", 50), strings.Repeat("t", 50)); len(got) != 64 {
		t.Errorf("not capped at 64: %d", len(got))
	}
}

func TestHTTPTools(t *testing.T) {
	for _, sse := range []bool{false, true} {
		f := &fakeMCP{sse: sse}
		h, _, err := register(t, `{"mcpServers":{"fake":{"url":"`+serve(t, f)+`"}}}`)
		if err != nil {
			t.Fatal(err)
		}
		if got := names(h); got != "echo,fail,pic" {
			t.Fatalf("sse=%v: tools %q (the second tools/list page must be read)", sse, got)
		}
		ts, ctx := tools(h), context.Background()
		if e := ts["fake__echo"]; e.Description != "echoes" || string(e.Schema) != `{"type":"object"}` || e.Server != "" {
			t.Fatalf("sse=%v: echo is %+v", sse, e)
		}
		if out, _, err := ts["fake__echo"].Run(ctx, `{"q":"weather"}`); err != nil || out != `echo {"q":"weather"}` {
			t.Fatalf("sse=%v: echo = %q, %v", sse, out, err)
		}
		if out, _, err := ts["fake__fail"].Run(ctx, ""); err == nil || out != "error: it broke" {
			t.Errorf("sse=%v: fail = %q, %v", sse, out, err)
		}
		if out, pics, _ := ts["fake__pic"].Run(ctx, "{}"); out != "[non-text content omitted]\n[image sent to the chat]\ncaption" ||
			len(pics) != 1 || len(pics[0].Data) == 0 || pics[0].Caption != "" || pics[0].HistoryNote != "" {
			t.Errorf("sse=%v: pic = %q, %+v", sse, out, pics)
		}
		if out, _, err := ts["fake__echo"].Run(ctx, "not json"); err == nil || !strings.Contains(out, "JSON object") {
			t.Errorf("sse=%v: bad arguments = %q, %v", sse, out, err)
		}
		if f.badCalls != 0 {
			t.Errorf("sse=%v: %d requests without the session id", sse, f.badCalls)
		}
	}
}

// allowed_tools is a whitelist and blocked_tools a blacklist; without
// either, every tool is offered. An empty allowed_tools offers none, so the
// server has no group.
func TestToolLists(t *testing.T) {
	for _, tc := range []struct {
		name, lists, want string
	}{
		{"neither", ``, "echo,fail,pic"},
		{"whitelist", `,"allowed_tools":["echo"]`, "echo"},
		{"empty whitelist", `,"allowed_tools":[]`, ""},
		{"blacklist", `,"blocked_tools":["fail"]`, "echo,pic"},
		{"both", `,"allowed_tools":["echo","fail"],"blocked_tools":["fail"]`, "echo"},
	} {
		h, _, err := register(t, `{"mcpServers":{"fake":{"url":"`+serve(t, &fakeMCP{})+`"`+tc.lists+`}}}`)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := names(h); got != tc.want {
			t.Errorf("%s: tools %q, want %q", tc.name, got, tc.want)
		}
		if tc.want == "" && len(h.ToolGroups()) != 0 {
			t.Errorf("%s: a group with no tools", tc.name)
		}
	}
}

// When the server forgets the session, the client starts a new one, once.
func TestHTTPSessionExpiry(t *testing.T) {
	f := &fakeMCP{}
	h, _, err := register(t, `{"mcpServers":{"fake":{"url":"`+serve(t, f)+`","allowed_tools":["echo"]}}}`)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.session = "rotated"
	f.mu.Unlock()
	if out, _, err := tools(h)["fake__echo"].Run(context.Background(), "{}"); err != nil || out != "echo {}" {
		t.Fatalf("after expiry: %q, %v", out, err)
	}
	if f.inits != 2 {
		t.Fatalf("%d initializations, want 2", f.inits)
	}
}

func TestStartupErrors(t *testing.T) {
	url, url2 := serve(t, &fakeMCP{}), serve(t, &fakeMCP{})
	if _, _, err := register(t, `{"mcpServers":{"fake":{"url":"`+url+`","allowed_tools":["echo","typo"]}}}`); err == nil {
		t.Error("a whitelisted tool the server lacks must be an error")
	}
	if _, _, err := register(t, `{"mcpServers":{"fake":{"url":"`+url+`","blocked_tools":["typo"]}}}`); err == nil {
		t.Error("a blacklisted tool the server lacks must be an error, or a typo would leave the real tool offered")
	}
	if _, _, err := register(t, `{"mcpServers":{"a.b":{"url":"`+url+`","allowed_tools":["echo"]},"a_b":{"url":"`+url2+`","allowed_tools":["echo"]}}}`); err == nil {
		t.Error("two tools with the same model name must be an error")
	}
	// An unreachable server is left out and logged, and the bot still starts.
	gone := httptest.NewServer(&fakeMCP{})
	gone.Close()
	h, log, err := register(t, `{"mcpServers":{"gone":{"url":"`+gone.URL+`","allowed_tools":["echo"]}}}`)
	if err != nil || len(h.ToolGroups()) != 0 {
		t.Fatalf("unreachable server: %+v, %v", h.ToolGroups(), err)
	}
	if !strings.Contains(log.String(), "<3>MCP server gone unavailable, its tools are left out: ") || strings.Contains(log.String(), "MCP tools:") {
		t.Fatalf("log %q", log.String())
	}
}

// Each server is its own group, named and listed by its tools then its
// commands, with its prompt: a server's own instructions are not used.
// keywords gate it on whole words, ignoring capitals. Each server is told
// the bot's version.
func TestToolGroups(t *testing.T) {
	scoutMCP := &fakeMCP{instructions: "Use scout for everything."}
	h, log, err := register(t, `{"mcpServers":{
		"yahoo":{"url":"`+serve(t, &fakeMCP{instructions: "ignored"})+`","allowed_tools":["echo","pic"],
			"instructions":"Use yahoo for prices.","keywords":["stock"," AAPL ",""]},
		"scout":{"url":"`+serve(t, scoutMCP)+`","allowed_tools":["fail"],
			"chat_commands":{"scout":{"tool":"fail"}}},
		"plain":{"url":"`+serve(t, &fakeMCP{})+`","allowed_tools":["echo"],"keywords":[]}}}`,
		func(h *featuretest.Host) { h.Bot.Version = "v1.2.3" })
	if err != nil {
		t.Fatal(err)
	}
	if scoutMCP.version != "v1.2.3" {
		t.Errorf("clientInfo version %q", scoutMCP.version)
	}
	gs := h.ToolGroups()
	if len(gs) != 3 {
		t.Fatalf("groups %+v", gs)
	}
	plain, scout, yahoo := gs[0], gs[1], gs[2] // in name order
	if plain.Listing != "plain: echo" || plain.System != "" || plain.Keywords != nil ||
		scout.Listing != "scout: fail, /scout" || !slices.Equal(scout.Names, []string{"fail", "/scout"}) ||
		scout.System != "Use scout for everything." || scout.Keywords != nil || // the server's own instructions
		yahoo.Listing != "yahoo: echo, pic" || !slices.Equal(yahoo.Names, []string{"echo", "pic"}) || yahoo.System != "Use yahoo for prices." {
		t.Fatalf("groups %+v", gs)
	}
	if len(scout.Tools) != 1 || scout.Tools[0].Name != "scout__fail" {
		t.Fatalf("scout's tools %+v", scout.Tools)
	}
	for text, want := range map[string]bool{
		"what's the Stock price?": true, "aapl now": true, "(AAPL)": true,
		"stocks": false, "stockpile": false, "hello": false,
	} {
		if got := yahoo.Keywords.MatchString(text); got != want {
			t.Errorf("keywords on %q: %v", text, got)
		}
	}
	for _, want := range []string{"<5>MCP tools: echo, fail, echo, pic\n", "<5>chat commands: /scout\n"} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log %q, want %q", log.String(), want)
		}
	}
}

// chat_commands in a server's config become commands for the tools it
// offers; a bad or taken name, or a tool that isn't offered, is logged and
// skipped. A command forces its tool, and with require_arg a bare one gets
// a usage reply. Without a version, the bot is "dev" to a server.
func TestChatCommands(t *testing.T) {
	fake := &fakeMCP{}
	h, log, err := register(t, `{"mcpServers":{"fake":{"url":"`+serve(t, fake)+`","allowed_tools":["echo","pic"],
		"chat_commands":{
			"Echo":{"tool":"echo","require_arg":true},
			"/pic":{"tool":"pic"},
			"example":{"tool":"echo"},
			"bad name":{"tool":"echo"},
			"fail":{"tool":"fail"}}}}}`,
		func(h *featuretest.Host) {
			feature.Command(h, "example", func(context.Context, feature.Message, string) feature.Action { return feature.Action{} })
		})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(h); got != "echo,pic,/echo,/pic" || fake.version != "dev" {
		t.Fatalf("names %q, version %q", got, fake.version)
	}
	for _, want := range []string{
		`<4>MCP server fake: chat command "bad name" is not a free name`,
		`<4>MCP server fake: chat command "example" is not a free name`,
		`<4>MCP server fake: chat command /fail names tool "fail", which isn't offered; skipped`,
		"<5>chat commands: /echo, /pic\n",
	} {
		if !strings.Contains(log.String(), want) {
			t.Errorf("log %q, want %q", log.String(), want)
		}
	}
	dm := func(text string) feature.Message {
		return feature.Message{Chat: feature.Chat{User: "alice"}, Sender: "alice", Name: "Alice", Text: text, Addressed: true}
	}
	ctx := context.Background()
	for text, want := range map[string]feature.Action{
		"/echo what's up":  {ToolChoice: "fake__echo"},
		"/ECHO":            {Answer: "/echo needs something after it."},
		"/pic":             {ToolChoice: "fake__pic"},
		"/pic of a cat":    {ToolChoice: "fake__pic"},
		"/fail it":         {},
		"echo what's up":   {},
		"/echoes whatever": {},
	} {
		if got := h.Message(ctx, dm(text)); got != want {
			t.Errorf("%q: %+v, want %+v", text, got, want)
		}
	}
	group := feature.Message{Chat: feature.Chat{GroupID: "g"}, Sender: "bob", Name: "Bob", Text: "/echo hi"}
	if got := h.Message(ctx, group); got != (feature.Action{}) {
		t.Errorf("unaddressed: %+v", got)
	}
}

// While the model can't call tools, no command is registered or logged, so
// its message goes to the model as it is.
func TestChatCommandsToolsOff(t *testing.T) {
	h, log, err := register(t, `{"mcpServers":{"fake":{"url":"`+serve(t, &fakeMCP{})+`","allowed_tools":["echo"],
		"chat_commands":{"echo":{"tool":"echo"}}}}}`, func(h *featuretest.Host) { h.Bot.CallsTools = false })
	if err != nil {
		t.Fatal(err)
	}
	if got := names(h); got != "echo" || strings.Contains(log.String(), "chat commands") {
		t.Fatalf("names %q, log %q", got, log.String())
	}
	m := feature.Message{Chat: feature.Chat{User: "alice"}, Sender: "alice", Name: "Alice", Text: "/echo hi", Addressed: true}
	if got := h.Message(context.Background(), m); got != (feature.Action{}) {
		t.Errorf("tools off: %+v", got)
	}
}

// TestHelperProcess is not a real test: run as a subprocess with
// MCP_HELPER=1, it is a stdio MCP server with two tools. env reports what the
// process can see; exit makes it quit after answering.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("MCP_HELPER") != "1" {
		return
	}
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		json.Unmarshal(sc.Bytes(), &req)
		if req.ID == nil {
			continue
		}
		result := `{}`
		switch req.Method {
		case "initialize":
			fmt.Fprintln(os.Stderr, "loaded Alice's notes")
			result = `{"protocolVersion":"` + protocol + `","capabilities":{"tools":{}},"serverInfo":{"name":"helper","version":"1"},"instructions":"Local files."}`
		case "tools/list":
			result = `{"tools":[{"name":"env"},{"name":"exit"}]}`
		case "tools/call":
			text, _ := json.Marshal("key=" + os.Getenv("MODEL_API_KEY") + " marker=" + os.Getenv("MCP_HELPER"))
			result = `{"content":[{"type":"text","text":` + string(text) + `}]}`
		}
		fmt.Printf(`{"jsonrpc":"2.0","id":%d,"result":%s}`+"\n", *req.ID, result)
		if req.Params.Name == "exit" {
			os.Exit(0)
		}
	}
	os.Exit(0)
}

// A stdio server sees only its configured environment, never the bot's API
// key, is restarted when it exits, and is stopped when the bot stops.
func TestStdio(t *testing.T) {
	var log logx.Buffer
	defer logx.SetOutput(&log, &log, true)()
	t.Setenv("MODEL_API_KEY", "secret-key")
	ctx := context.Background()
	self, _ := json.Marshal(os.Args[0])
	h := featuretest.New()
	h.Bot.Model = "x/test-model"
	h.Redactor = func(s string) string { return strings.ReplaceAll(s, "Alice", "redacted") }
	h.Settings["MCP_CONFIG"] = writeConfig(t, `{"mcpServers":{"local":{"command":`+string(self)+
		`,"args":["-test.run=^TestHelperProcess$"],"env":{"MCP_HELPER":"1"},"allowed_tools":["env","exit"]}}}`)
	if err := Register(h); err != nil {
		t.Fatal(err)
	}
	defer h.Stop()                                                            // its stdin stays open, so this returns only once SIGTERM stops it
	if gs := h.ToolGroups(); len(gs) != 1 || gs[0].System != "Local files." { // the server's own instructions
		t.Fatalf("groups %+v", gs)
	}
	ts := tools(h)
	if out, _, err := ts["local__env"].Run(ctx, "{}"); err != nil || out != "key= marker=1" {
		t.Fatalf("env = %q, %v", out, err)
	}
	// Its own output is scrubbed as it passes, before it is logged.
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(log.String(), "<7>MCP server local: loaded redacted's notes\n"); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("logs:\n%s", log.String())
		}
	}
	if strings.Contains(log.String(), "Alice") {
		t.Fatalf("a name reached the log:\n%s", log.String())
	}
	ts["local__exit"].Run(ctx, "{}")
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, _, err := ts["local__env"].Run(ctx, "{}")
		if err == nil && out == "key= marker=1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not restarted: %q, %v", out, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// One event split over several data: lines is joined with newlines.
func TestReadSSEMultiLine(t *testing.T) {
	stream := "event: message\ndata: {\"jsonrpc\":\"2.0\",\ndata: \"id\":7,\"result\":{\"ok\":true}}\n\n"
	m, err := readSSE(strings.NewReader(stream), 7)
	if err != nil || string(m.Result) != `{"ok":true}` {
		t.Fatalf("got %+v, %v", m, err)
	}
}

// A server's config instructions win over its own, even when empty; its
// own are capped.
func TestInstructions(t *testing.T) {
	empty, mine := "", " Mine. "
	long := strings.Repeat("x", maxInstructions+50)
	for _, c := range []struct {
		cfg       *string
		own, want string
	}{
		{nil, "Theirs.", "Theirs."},
		{&mine, "Theirs.", "Mine."},
		{&empty, "Theirs.", ""},
		{nil, "", ""},
	} {
		if got := instructions(c.cfg, c.own); got != c.want {
			t.Errorf("instructions(%v, %q) = %q, want %q", c.cfg, c.own, got, c.want)
		}
	}
	if got := []rune(instructions(nil, long)); len(got) != maxInstructions+1 || got[maxInstructions] != '…' {
		t.Errorf("capped to %d runes", len(got))
	}
}

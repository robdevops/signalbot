package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
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
	"unicode/utf8"

	ext "github.com/robdevops/signalbot/addon"
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

// names is every offered tool and command, from each server's listing
// ("server: tool, /command").
func names(h *featuretest.Host) string {
	var out []string
	for _, g := range h.ToolGroups() {
		_, list, _ := strings.Cut(g.Listing, ": ")
		out = append(out, strings.Split(list, ", ")...)
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
	network  string // the chat network, as initialize offered it
	inits    int
	badCalls int    // requests that carried the wrong session id
	feed     string // the text of resource feed://items
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
		var p struct {
			ClientInfo   struct{ Version string }
			Capabilities struct {
				Experimental map[string]struct{ Network string }
			}
		}
		json.Unmarshal(req.Params, &p)
		f.inits, f.version = f.inits+1, p.ClientInfo.Version
		f.network = p.Capabilities.Experimental[ext.KeyAddon].Network
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
			result = `{"tools":[{"name":"fail","inputSchema":{"type":"object"}},{"name":"pic"},{"name":"worded"}]}`
		}
	case "resources/read":
		var p struct {
			URI string `json:"uri"`
		}
		json.Unmarshal(req.Params, &p)
		if p.URI == "json://now" {
			result = `{"contents":[{"uri":"json://now","mimeType":"application/json","text":"{\"ok\":true}"},{"uri":"json://now","mimeType":"text/markdown","text":"**All good**"}]}`
			break
		}
		if p.URI == "feed://items" { // under mu
			text, _ := json.Marshal(f.feed)
			result = `{"contents":[{"uri":"feed://items","mimeType":"application/json","text":` + string(text) + `}]}`
			break
		}
		result = `{"contents":[{"uri":"status://now","mimeType":"text/plain","text":"All good"},{"uri":"status://now","mimeType":"image/png","blob":"` + tinyPNG() + `"}]}`
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
		case "worded":
			result = `{"content":[{"type":"text","text":"{\"rain\":3}"},{"type":"text","text":"for the model","annotations":{"audience":["assistant"]}},` +
				`{"type":"image","mimeType":"image/png","data":"` + tinyPNG() + `","annotations":{"audience":["assistant"]}},` +
				`{"type":"text","text":"3 mm of rain","annotations":{"audience":["user"]}}]}`
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

// With no model, no server but an addon or one with direct commands is
// started, and the log says so.
func TestNoModel(t *testing.T) {
	var started atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { started.Store(true) }))
	t.Cleanup(srv.Close)
	h, log, err := register(t, `{"mcpServers":{"w":{"url":"`+srv.URL+`"}}}`, func(h *featuretest.Host) { h.Bot = feature.Info{} })
	if err != nil || len(h.ToolGroups()) != 0 || started.Load() {
		t.Fatalf("groups %+v, %v, started %v", h.ToolGroups(), err, started.Load())
	}
	if l := log.String(); l != "<4>MCP_CONFIG is set but no model is configured, so only addon servers and those with direct commands were started\n" {
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
	// A JSON mistake says where; a setting outside its server's braces says so.
	for bad, want := range map[string]string{
		"{\n  \"mcpServers\": {\n    \"y\": {\"url\": \"u\"\n      \"keywords\": [\"k\"]}}}": "line 4, column 7: invalid character '\"' after object key:value pair",
		`{"mcpServers":{"y":{"url":"u"},"keywords":["stock"]}}`:                              `MCP server "keywords" must be an object, not array: is "keywords" a setting outside its server's braces?`,
	} {
		if _, err := load(writeConfig(t, bad)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", bad, err, want)
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
	// ${NAME} in a url, header, arg or env value is the bot's environment
	// variable; one that isn't set is an error naming it, never its value,
	// and never an empty token sent. $NAME alone is left as it is.
	t.Setenv("MCP_TEST_TOKEN", "s3cret")
	t.Setenv("MCP_TEST_HOST", "x")
	tokens := `{"mcpServers":{"w":{"url":"https://${MCP_TEST_HOST}/mcp","headers":{"Authorization":"Bearer ${MCP_TEST_TOKEN}","X":"$MCP_TEST_TOKEN"}},` +
		`"l":{"command":"x","args":["--key=${MCP_TEST_TOKEN}"],"env":{"KEY":"${MCP_TEST_TOKEN}${MCP_TEST_HOST}"}}}}`
	s, err = load(writeConfig(t, tokens))
	if err != nil || s["w"].URL != "https://x/mcp" || s["w"].Headers["Authorization"] != "Bearer s3cret" || s["w"].Headers["X"] != "$MCP_TEST_TOKEN" ||
		s["l"].Args[0] != "--key=s3cret" || s["l"].Env["KEY"] != "s3cretx" {
		t.Fatalf("expanded: %+v, %v", s, err)
	}
	unset := writeConfig(t, `{"mcpServers":{"w":{"url":"u","headers":{"A":"${MCP_TEST_UNSET}"}}}}`)
	if _, err := load(unset); err == nil || err.Error() != unset+`: MCP server "w": ${MCP_TEST_UNSET} isn't set in the bot's environment` {
		t.Fatalf("unset: %v", err)
	}
	// An unknown setting, in a server or a chat command, is refused with the
	// settings it could be, and both lists at once too; keys other clients
	// write are ignored, so a pasted entry starts.
	for bad, want := range map[string]string{
		`{"mcpServers":{"w":{"url":"u","feed":{"command":"a"}}}}`:                                                            `: MCP server "w": feed needs a resource`,
		`{"mcpServers":{"w":{"url":"u","feed":{"uri":"a"}}}}`:                                                                `: MCP server "w": feed: unknown setting "uri" (settings are resource, command)`,
		`{"mcpServers":{"w":{"url":"u","allowedTools":["t"]}}}`:                                                              `: MCP server "w": unknown setting "allowedTools" (settings are url, headers, command, args, env, allowed_tools, blocked_tools, instructions, keywords, chat_commands, disabled, addon, name, feed, keyboard)`,
		`{"mcpServers":{"w":{"url":"u","chat_commands":{"x":{"tool":"t","requireArg":true}}}}}`:                              `: MCP server "w": chat command "x": unknown setting "requireArg" (settings are tool, require_arg, example, direct, timeout, args, arg, values, resource)`,
		`{"mcpServers":{"w":{"url":"u","chat_commands":{"x":{"tool":"t","timeout":"30s"}}}}}`:                                `: MCP server "w": chat command "x" has a timeout, which only a direct command takes`,
		`{"mcpServers":{"w":{"command":"c","addon":{},"chat_commands":{"x":{"tool":"t","direct":true,"timeout":"3m"}}}}}`:    `: MCP server "w": chat command "x": timeout "3m" isn't a time from 1s to 2m, such as 15s or 1m`,
		`{"mcpServers":{"w":{"command":"c","addon":{},"chat_commands":{"x":{"tool":"t","direct":true,"timeout":"500ms"}}}}}`: `: MCP server "w": chat command "x": timeout "500ms" isn't a time from 1s to 2m, such as 15s or 1m`,
		`{"mcpServers":{"w":{"url":"u","allowed_tools":[],"blocked_tools":["t"]}}}`:                                          `: MCP server "w": has both allowed_tools and blocked_tools; use one`,
	} {
		p := writeConfig(t, bad)
		if _, err := load(p); err == nil || err.Error() != p+want {
			t.Errorf("%s: got %v", bad, err)
		}
	}
	if s, err := load(writeConfig(t, `{"mcpServers":{"w":{"type":"http","url":"u","timeout":60,"alwaysAllow":[],"autoApprove":[],"description":"d","transport":"x"}}}`)); err != nil || len(s) != 1 {
		t.Fatalf("ignored keys: %+v, %v", s, err)
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
		if got := names(h); got != "echo,fail,pic,worded" {
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
		{"neither", ``, "echo,fail,pic,worded"},
		{"whitelist", `,"allowed_tools":["echo"]`, "echo"},
		{"empty whitelist", `,"allowed_tools":[]`, ""},
		{"blacklist", `,"blocked_tools":["fail"]`, "echo,pic,worded"},
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

// A tool name the server lacks, in either list, or two tools with one name
// for the model, stops the bot with the file's path, the list, and what the
// server has (a typo in blocked_tools would leave the real tool offered).
func TestStartupErrors(t *testing.T) {
	url, url2 := serve(t, &fakeMCP{}), serve(t, &fakeMCP{})
	if h, _, err := register(t, `{"mcpServers":{"fake":{"url":"`+url+`","allowed_tools":["echo","typo"]}}}`); err == nil ||
		err.Error() != h.Settings["MCP_CONFIG"]+`: MCP server "fake": allowed_tools names "typo", which the server doesn't have; its tools are: echo, fail, pic, worded` {
		t.Errorf("whitelist: %v", err)
	}
	if _, _, err := register(t, `{"mcpServers":{"fake":{"url":"`+url+`","blocked_tools":["typo"]}}}`); err == nil || !strings.Contains(err.Error(), `blocked_tools names "typo"`) {
		t.Errorf("blacklist: %v", err)
	}
	if _, _, err := register(t, `{"mcpServers":{"a.b":{"url":"`+url+`","allowed_tools":["echo"]},"a_b":{"url":"`+url2+`","allowed_tools":["echo"]}}}`); err == nil ||
		!strings.HasSuffix(err.Error(), `: MCP server "a_b": tool "echo" becomes "a_b__echo" for the model, as does server "a.b"'s tool "echo"; block one`) {
		t.Errorf("clash: %v", err)
	}
	var many []tool
	for i := range 25 {
		many = append(many, tool{Name: fmt.Sprintf("t%02d", i)})
	}
	if l := toolList(many); !strings.HasPrefix(l, "t00, t01") || !strings.HasSuffix(l, "t19, …") {
		t.Errorf("tool list %q", l)
	}
	// An unreachable server is left out and logged, and the bot still starts.
	gone := httptest.NewServer(&fakeMCP{})
	gone.Close()
	h, log, err := register(t, `{"mcpServers":{"gone":{"url":"`+gone.URL+`","allowed_tools":["echo"]}}}`)
	if err != nil || len(h.ToolGroups()) != 0 {
		t.Fatalf("unreachable server: %+v, %v", h.ToolGroups(), err)
	}
	if !strings.Contains(log.String(), "<3>MCP server gone unavailable, its tools are left out: ") || strings.Contains(log.String(), " MCP: ") {
		t.Fatalf("log %q", log.String())
	}
}

// Each server is its own group, named and listed by its tools then its
// commands, its {tools} label ending " MCP" unless its name has it, with its prompt: a server's own instructions are not used.
// keywords gate it on whole words, ignoring capitals. Each server is told
// the bot's version.
func TestToolGroups(t *testing.T) {
	scoutMCP := &fakeMCP{instructions: "Use scout for everything."}
	h, log, err := register(t, `{"mcpServers":{
		"yahoo":{"url":"`+serve(t, &fakeMCP{instructions: "ignored"})+`","allowed_tools":["echo","pic"],
			"instructions":"Use yahoo for prices.","keywords":["stock"," AAPL ",""]},
		"scout":{"url":"`+serve(t, scoutMCP)+`","allowed_tools":["fail"],
			"chat_commands":{"scout":{"tool":"fail"}}},
		"plain_Mcp":{"url":"`+serve(t, &fakeMCP{})+`","allowed_tools":["echo"],"keywords":[]}}}`,
		func(h *featuretest.Host) { h.Bot.Version, h.Bot.Network = "v1.2.3", "signal" })
	if err != nil {
		t.Fatal(err)
	}
	if scoutMCP.version != "v1.2.3" || scoutMCP.network != "signal" {
		t.Errorf("clientInfo version %q, network %q", scoutMCP.version, scoutMCP.network)
	}
	gs := h.ToolGroups()
	if len(gs) != 3 {
		t.Fatalf("groups %+v", gs)
	}
	plain, scout, yahoo := gs[0], gs[1], gs[2] // in name order
	if plain.Listing != "plain_Mcp: echo" || plain.System != "" || plain.Keywords != nil ||
		scout.Listing != "scout: fail, /scout" || !slices.Equal(scout.Names, []string{"**scout MCP**: fail, /scout"}) ||
		scout.System != "Use scout for everything." || scout.Keywords != nil || // the server's own instructions
		yahoo.Listing != "yahoo: echo, pic" || !slices.Equal(yahoo.Names, []string{"**yahoo MCP**: echo, pic"}) || !slices.Equal(plain.Names, []string{"**plain_Mcp**: echo"}) || yahoo.System != "Use yahoo for prices." {
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
	for _, want := range []string{"<5>plain_Mcp: echo\n", "<5>scout MCP: fail\n", "<5>yahoo MCP: echo, pic\n", "<5>chat commands: /scout\n"} {
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
			"/pic":{"tool":"pic","require_arg":true,"example":"/pic of a cat."},
			"example":{"tool":"echo"},
			"bad name":{"tool":"echo"},
			"fail":{"tool":"fail"}}}}}`,
		func(h *featuretest.Host) {
			h.Command("example", func(context.Context, feature.Message, string) feature.Action { return feature.Action{} })
		})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(h); got != "echo,pic,/echo,/pic" || fake.version != "dev" {
		t.Fatalf("names %q, version %q", got, fake.version)
	}
	for _, want := range []string{
		`<4>MCP server fake: chat command skipped: command /bad name: a name is 1 to 32 lowercase letters, digits, _ or -`,
		`<4>MCP server fake: chat command skipped: command /example is already taken`,
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
		"/ECHO":            {Answer: "Put what you want after it, like /echo <your request>. Or just ask me in plain words."},
		"/pic":             {Answer: "Put what you want after it, like /pic <your request>. Try: /pic of a cat. Or just ask me in plain words."},
		"/pic of a cat":    {ToolChoice: "fake__pic"},
		"/fail it":         {},
		"echo what's up":   {},
		"/echoes whatever": {},
	} {
		if got := h.Message(ctx, dm(text)); got.Answer != want.Answer || got.ToolChoice != want.ToolChoice || got.IsZero() != want.IsZero() {
			t.Errorf("%q: %+v, want %+v", text, got, want)
		}
	}
	group := feature.Message{Chat: feature.Chat{GroupID: "g"}, Sender: "bob", Name: "Bob", Text: "/echo hi"}
	if got := h.Message(ctx, group); !got.IsZero() {
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
	if got := h.Message(context.Background(), m); !got.IsZero() {
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
	defer func(d time.Duration) { restartDelay = d }(restartDelay)
	restartDelay = 10 * time.Millisecond
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
	defer h.Stop()                                                            // its stdin is closed, and it exits
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
	long := strings.Repeat("x", maxInstructions+50)
	for _, c := range []struct {
		cfg       *string
		own, want string
	}{
		{nil, "Theirs.", "Theirs."},
		{new(" Mine. "), "Theirs.", "Mine."},
		{new(""), "Theirs.", ""},
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

// A local server that exits is restarted after a second at first.
func TestRestartDelay(t *testing.T) {
	if restartDelay != time.Second {
		t.Fatalf("restart delay %v", restartDelay)
	}
}

// failing is a server that answers every call with err.
type failing struct{ err error }

func (f failing) call(context.Context, string, any) (json.RawMessage, error) { return nil, f.err }
func (f failing) notify(context.Context, string, any) error                  { return f.err }

// A call that times out tells the model so, and what to try; any other
// failure says only that the tool couldn't be reached.
func TestCallFailures(t *testing.T) {
	ctx := context.Background()
	if out, _, err := call(ctx, failing{fmt.Errorf("post: %w", context.DeadlineExceeded)}, "t", "{}"); err == nil ||
		out != "error: the tool didn't answer within 30s; try a narrower request or later" {
		t.Errorf("timeout: %q", out)
	}
	if out, _, err := call(ctx, failing{errors.New("connection refused")}, "t", "{}"); err == nil || out != "error: the tool could not be reached" {
		t.Errorf("unreachable: %q", out)
	}
}

// examples/signalbot-mcp.json is the docs' example, as written there, and
// it loads: the disabled server is dropped, the others kept, one an addon.
func TestExampleFile(t *testing.T) {
	example, err := os.ReadFile("../../../examples/signalbot-mcp.json")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := os.ReadFile("../../../docs/FILES_LINKS_TOOLS_MCP.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc), "```json\n"+string(example)+"```\n") {
		t.Error("examples/signalbot-mcp.json differs from the example in docs/FILES_LINKS_TOOLS_MCP.md")
	}
	t.Setenv("WEATHER_TOKEN", "x")
	s, err := load("../../../examples/signalbot-mcp.json")
	if err != nil || len(s) != 3 || s["weather"].Headers["Authorization"] != "Bearer x" || s["weather"].ChatCommands["forecast"].Example == "" ||
		s["geo"].Addon == nil || !s["geo"].Addon.Watch || !s["geo"].ChatCommands["geo"].Direct || s["geo"].Addon.Model != nil ||
		s["quiz"].Addon == nil || s["quiz"].Addon.Model == nil || *s["quiz"].Addon.Model != (modelConfig{30, 4000}) {
		t.Fatalf("loaded %+v, %v", s, err)
	}
}

// A plain server's direct command: the bot calls its tool with the
// command's args, the text after it as its arg (one of its values), or
// reads a resource, and sends the result without the model, named by the
// server's "name" when it fails.
func TestPlainDirect(t *testing.T) {
	h, log, err := register(t, `{"mcpServers":{"fake":{"url":"`+serve(t, &fakeMCP{})+`","name":"Weather","allowed_tools":["echo"],
		"chat_commands":{
			"rain":{"tool":"echo","direct":true,"args":{"readings":["rain"]},"arg":"period","values":["1d","7d"]},
			"say":{"tool":"echo","direct":true,"arg":"what"},
			"status":{"resource":"status://now","direct":true},
			"photo":{"tool":"pic","direct":true},
			"broken":{"tool":"fail","direct":true},
			"ghost":{"tool":"nope","direct":true}}}}}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), `MCP server fake: chat command /ghost names tool "nope", which the server doesn't have; skipped`) {
		t.Errorf("log %q", log.String())
	}
	dm := func(text string) feature.Message {
		return feature.Message{Chat: feature.Chat{User: "alice"}, Sender: "alice", Text: text, Addressed: true}
	}
	ctx := logx.With(context.Background(), logx.For("slack")) // as the core's: the lines name the network
	for text, want := range map[string]string{
		"/rain":             `echo {"readings":["rain"]}`,
		"/rain 7D":          `echo {"period":"7d","readings":["rain"]}`,
		"/rain 2w":          "Use /rain alone, or with one of: 1d, 7d.",
		"/say  hello there": `echo {"what":"hello there"}`,
		"/broken":           "Weather couldn't do that just now.",
		"/ghost":            "",
	} {
		if got := h.Message(ctx, dm(text)); got.Answer != want || got.ToolChoice != "" {
			t.Errorf("%q: %+v, want %q", text, got, want)
		}
	}
	if got := h.Message(ctx, dm("/status")); got.Answer != "All good" || len(got.Pictures) != 1 {
		t.Errorf("status: %q, %d pictures", got.Answer, len(got.Pictures))
	}
	if got := h.Message(ctx, dm("/photo")); got.Answer != "caption" || len(got.Pictures) != 1 {
		t.Errorf("photo: %q, %d pictures", got.Answer, len(got.Pictures))
	}
	if !strings.Contains(log.String(), "<6>Slack: MCP server fake: /status read a resource: ok in ") || strings.Contains(log.String(), "status://now") {
		t.Errorf("log %q", log.String())
	}
}

// A server with only direct commands starts with no model, as they need
// none; one with neither those nor an addon block doesn't.
func TestPlainDirectNoModel(t *testing.T) {
	var log logx.Buffer
	t.Cleanup(logx.SetOutput(&log, &log, true))
	h := featuretest.New()
	h.Settings["MCP_CONFIG"] = writeConfig(t, `{"mcpServers":{
		"weather":{"url":"`+serve(t, &fakeMCP{})+`","chat_commands":{"status":{"resource":"status://now","direct":true}}},
		"plain":{"url":"`+serve(t, &fakeMCP{})+`"}}}`)
	t.Cleanup(h.Stop)
	if err := Register(h); err != nil {
		t.Fatal(err)
	}
	got := h.Message(context.Background(), feature.Message{Chat: feature.Chat{User: "a"}, Sender: "a", Text: "/status", Addressed: true})
	if got.Answer != "All good" || strings.Contains(log.String(), "plain MCP") {
		t.Fatalf("got %+v; log %q", got, log.String())
	}
}

// A plain direct command's settings are checked at startup.
func TestPlainDirectSettings(t *testing.T) {
	url := serve(t, &fakeMCP{})
	for cmd, want := range map[string]string{
		`{"tool":"echo","args":{"a":1}}`:                   "are for a direct command of a server without an addon block",
		`{"resource":"x://y","tool":"echo","direct":true}`: "a resource takes no tool, arg or args",
		`{"tool":"echo","direct":true,"values":["a"]}`:     "values without the arg they are for",
		`{"direct":true}`: "needs a tool",
	} {
		_, _, err := register(t, `{"mcpServers":{"fake":{"url":"`+url+`","chat_commands":{"c":`+cmd+`}}}}`)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", cmd, err, want)
		}
	}
}

// Where the network has buttons, a plain direct command's post has one per
// value, the current one marked, and refresh; a press makes the call
// again and edits the post, also after a restart. Without buttons, none.
func TestRerun(t *testing.T) {
	url := serve(t, &fakeMCP{})
	config := `{"mcpServers":{"fake":{"url":"` + url + `","allowed_tools":["echo"],"chat_commands":{
		"rain":{"tool":"echo","direct":true,"args":{"period":"1d"},"arg":"period","values":["1d","7d"]}}}}}`
	dir := t.TempDir()
	buttons := func(h *featuretest.Host) { h.Bot.Caps.Buttons, h.Settings["STATE_DIRECTORY"] = true, dir }
	h, _, err := register(t, config, buttons)
	if err != nil {
		t.Fatal(err)
	}
	alice := feature.Chat{User: "alice"}
	ctx := context.Background()
	got := h.Message(ctx, feature.Message{Chat: alice, Sender: "alice", Text: "/rain", Addressed: true})
	labels := func(rows [][]feature.Button) (s []string) {
		for _, row := range rows {
			for _, b := range row {
				s = append(s, b.Label+"="+b.Data)
			}
		}
		return s
	}
	if l := labels(got.Markup.Buttons); !slices.Equal(l, []string{"• 1d=rerun=1d", "7d=rerun=7d", "🔄=rerun"}) || got.OnSent == nil {
		t.Fatalf("buttons %q", l)
	}
	got.OnSent([]int64{11}, false)
	if _, ok := h.Press(ctx, feature.Press{Chat: alice, Sent: 10, Data: "rerun=7d"}); ok {
		t.Error("a press under another message was taken")
	}
	if a, ok := h.Press(ctx, feature.Press{Chat: alice, Sent: 11, Data: "rerun=7d"}); !ok || a != "" {
		t.Fatalf("press: %q %v", a, ok)
	}
	p := h.Posts()
	if len(p) != 1 || p[0].Edit != 11 || p[0].Text != `echo {"period":"7d"}` || !slices.Equal(labels(p[0].Markup.Buttons), []string{"1d=rerun=1d", "• 7d=rerun=7d", "🔄=rerun"}) {
		t.Fatalf("posts %+v", p)
	}
	p[0].OnSent([]int64{12}, false) // as a network that sends an edit anew
	if a, ok := h.Press(ctx, feature.Press{Chat: alice, Sent: 12, Data: "rerun=2w"}); !ok || a != "That choice no longer works." {
		t.Errorf("unknown value: %q %v", a, ok)
	}
	h.Stop()

	h, _, err = register(t, config, buttons) // restarted
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := h.Press(ctx, feature.Press{Chat: alice, Sent: 11, Data: "rerun"}); ok {
		t.Error("a press under the post's old message was taken")
	}
	if _, ok := h.Press(ctx, feature.Press{Chat: alice, Sent: 12, Data: "rerun"}); !ok {
		t.Fatal("refresh after a restart not taken")
	}
	if p := h.Posts(); len(p) != 1 || p[0].Edit != 12 || p[0].Text != `echo {"period":"7d"}` {
		t.Fatalf("refresh %+v", p)
	}

	h, _, err = register(t, config) // a network without buttons
	if err != nil {
		t.Fatal(err)
	}
	if got := h.Message(ctx, feature.Message{Chat: alice, Sender: "alice", Text: "/rain 7d", Addressed: true}); got.Markup.Buttons != nil || got.Answer != `echo {"period":"7d"}` {
		t.Errorf("without buttons: %+v", got)
	}
}

// A feed's new items go to the chats subscribed, silently unless they
// turned the sound on, without the kinds they turned off, with a chart;
// none twice, across a restart; old ones never.
func TestFeed(t *testing.T) {
	old := feedEvery
	feedEvery = 20 * time.Millisecond
	t.Cleanup(func() { feedEvery = old })
	fake := &fakeMCP{}
	now := time.Now().UTC()
	item := func(id, kind, text string, at time.Time, chart bool) string {
		s := fmt.Sprintf(`{"id":%q,"kind":%q,"time":%q,"text":%q`, id, kind, at.Format(time.RFC3339), text)
		if chart {
			s += `,"chart":{"tool":"pic","arguments":{"period":"1d"}}`
		}
		return s + "}"
	}
	setFeed := func(identity string, items ...string) {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		fake.feed = `{"identity":"` + identity + `","kinds":[{"kind":"rain","label":"Rain","words":["showers"]},{"kind":"wind","label":"Wind"}],"items":[` + strings.Join(items, ",") + `]}`
	}
	setFeed("a", item("1", "rain", "Old rain", now.Add(-2*time.Hour), false), item("2", "rain", "Rain now", now, true))
	config := `{"mcpServers":{"eco":{"url":"` + serve(t, fake) + `","name":"Weather","feed":{"resource":"feed://items"}}}}`
	dir := t.TempDir()
	state := func(h *featuretest.Host) {
		h.Settings["STATE_DIRECTORY"], h.Shared = dir, []feature.Group{{ID: "g1", Name: "Garden", Admin: true}} // alice is its admin
	}
	h, _, err := register(t, config, state)
	if err != nil {
		t.Fatal(err)
	}
	alice, group := feature.Chat{User: "alice"}, feature.Chat{GroupID: "g1"}
	ctx := context.Background()
	ask := func(h *featuretest.Host, chat feature.Chat, text string) string {
		return h.Message(ctx, feature.Message{Chat: chat, Sender: "alice", Text: text, Addressed: true}).Answer
	}
	wait := func(h *featuretest.Host, n int) []feature.Post {
		t.Helper()
		for range 200 {
			if p := h.Posts(); len(p) >= n {
				return p
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatalf("posts %+v, want %d", h.Posts(), n)
		return nil
	}
	a := ""
	for range 200 { // the first read, for the kinds
		if a = ask(h, group, "/alerts"); strings.Contains(a, "Off: Rain, Wind") { // a group starts off
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(a, "🔔 Weather alerts in this chat\n✅ On: nothing\n🔕 Off: Rain, Wind") || !strings.Contains(a, "sound and a kind: rain, wind.") {
		t.Errorf("menu %q", a)
	}
	ask(h, alice, "/alerts on")
	ask(h, group, "/alerts on")
	ask(h, group, "/alerts sound on")
	if a := ask(h, group, "/alerts showers"); !strings.Contains(a, "On: Wind\n🔕 Off: Rain") || !strings.Contains(a, "With sound: Wind") {
		t.Errorf("group menu %q", a)
	}
	if a := ask(h, alice, "/alerts hail"); !strings.HasPrefix(a, "I don't know that one.") {
		t.Errorf("unknown kind %q", a)
	}
	time.Sleep(3 * feedEvery) // read already: what it had is old news to them
	if p := h.Posts(); len(p) != 0 {
		t.Fatalf("posted the items there before subscribing: %+v", p)
	}
	setFeed("a", item("2", "rain", "Rain now", now, true), item("3", "rain", "More rain", now, true), item("4", "wind", "Windy", now, false))
	p := wait(h, 3)
	time.Sleep(3 * feedEvery)
	if p = h.Posts(); len(p) != 3 {
		t.Fatalf("posts %+v", p)
	}
	byChat := map[string][]string{}
	for _, x := range p {
		byChat[x.Chat.Key()] = append(byChat[x.Chat.Key()], fmt.Sprintf("%s silent=%v pics=%d", x.Text, x.Markup.Silent, len(x.Pictures)))
	}
	if got := byChat["u:alice"]; !slices.Equal(got, []string{"More rain silent=true pics=1", "Windy silent=true pics=0"}) {
		t.Errorf("alice %q", got)
	}
	if got := byChat["g:g1"]; !slices.Equal(got, []string{"Windy silent=false pics=0"}) {
		t.Errorf("group %q", got)
	}
	h.Stop()

	h, _, err = register(t, config, state) // restarted: nothing again
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * feedEvery)
	if p := h.Posts(); len(p) != 0 {
		t.Fatalf("after a restart %+v", p)
	}
	setFeed("b", item("1", "wind", "New log", now, false)) // started over: ID 1 is new
	if p := wait(h, 2); len(p) != 2 || p[0].Text != "New log" || p[1].Text != "New log" {
		t.Fatalf("after a new identity %+v", p)
	}
	ask(h, alice, "/alerts off")
	if a := ask(h, alice, "/alerts"); !strings.Contains(a, "On: nothing") {
		t.Errorf("off: %q", a)
	}
}

// On a network with buttons, the menu has them, and a press changes the
// chat's subscription and redraws the menu in place.
func TestFeedButtons(t *testing.T) {
	fake := &fakeMCP{feed: `{"kinds":[{"kind":"rain","label":"Rain"}],"items":[]}`}
	config := `{"mcpServers":{"eco":{"url":"` + serve(t, fake) + `","feed":{"resource":"feed://items","command":"/eco"}}}}`
	h, _, err := register(t, config, func(h *featuretest.Host) { h.Bot.Caps.Buttons, h.Settings["STATE_DIRECTORY"] = true, t.TempDir() })
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	alice := feature.Chat{User: "alice"}
	for range 200 { // the first read, for the kinds
		if a := h.Message(ctx, feature.Message{Chat: alice, Sender: "alice", Text: "/eco on", Addressed: true}); strings.Contains(a.Answer, "Rain") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if a, ok := h.Press(ctx, feature.Press{Chat: alice, Sent: 7, Data: "feed=eco/k:rain"}); !ok || a != "" {
		t.Fatalf("press %q %v", a, ok)
	}
	p := h.Posts()
	if len(p) != 1 || p[0].Edit != 7 || !strings.Contains(p[0].Text, "Off: Rain") {
		t.Fatalf("posts %+v", p)
	}
	if b := p[0].Markup.Buttons; b[0][0].Label != "🔔 Alerts ▾" || b[1][0].Label != "R\u0336a\u0336i\u0336n\u0336" || b[1][0].Highlight {
		t.Fatalf("buttons %+v", b)
	}
	if a, ok := h.Press(ctx, feature.Press{Chat: alice, Sent: 7, Data: "feed=eco/k:hail"}); !ok || a != "That choice no longer works." {
		t.Errorf("unknown kind: %q %v", a, ok)
	}
}

// The servers' keyboards make /keyboard, in a DM on a network with a
// reply keyboard: their keys, a label once, three to a row.
func TestConfigKeyboard(t *testing.T) {
	url := serve(t, &fakeMCP{})
	config := `{"mcpServers":{
		"a":{"url":"` + url + `","keyboard":[["Status","/status"],["Rain","/rain 7d"],["Wind","/wind"],["Alerts","/alerts"]]},
		"b":{"url":"` + url + `","keyboard":[["Status","/other"]]}}}`
	h, _, err := register(t, config, func(h *featuretest.Host) { h.Bot.Caps.Keyboard = true })
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	got := h.Message(ctx, feature.Message{Chat: feature.Chat{User: "a"}, Sender: "a", Text: "/keyboard", Addressed: true})
	if k := got.Markup.Keyboard; len(k) != 2 || len(k[0]) != 3 || k[0][1] != (feature.Key{Label: "Rain", Command: "/rain 7d"}) || len(k[1]) != 1 {
		t.Fatalf("keyboard %+v", k)
	}
	if got := h.Message(ctx, feature.Message{Chat: feature.Chat{User: "a"}, Sender: "a", Text: "/keyboard off", Addressed: true}); !got.Markup.NoKeyboard {
		t.Errorf("off %+v", got)
	}
	if got := h.Message(ctx, feature.Message{Chat: feature.Chat{GroupID: "g"}, Sender: "a", Text: "/keyboard", Addressed: true}); got.Answer != "The keyboard is for DMs." {
		t.Errorf("group %+v", got)
	}

	h, _, err = register(t, config) // no reply keyboard: no command
	if err != nil {
		t.Fatal(err)
	}
	if got := h.Message(ctx, feature.Message{Chat: feature.Chat{User: "a"}, Sender: "a", Text: "/keyboard", Addressed: true}); !got.IsZero() {
		t.Errorf("without a keyboard %+v", got)
	}
	_, _, err = register(t, `{"mcpServers":{"a":{"url":"`+url+`","keyboard":[["Status","status"]]}}}`)
	if err == nil || !strings.Contains(err.Error(), "isn't a label") {
		t.Errorf("a bad key: %v", err)
	}
}

// A bare command whose default isn't one of its values refreshes bare; a
// press under an older message in a forum topic, which names the group
// alone, finds the post in its topic and edits it there.
func TestRerunDefaultAndTopic(t *testing.T) {
	config := `{"mcpServers":{"fake":{"url":"` + serve(t, &fakeMCP{}) + `","allowed_tools":["echo"],"chat_commands":{
		"rain":{"tool":"echo","direct":true,"args":{"period":"24h"},"arg":"period","values":["1d","7d"]}}}}}`
	h, _, err := register(t, config, func(h *featuretest.Host) { h.Bot.Caps.Buttons, h.Settings["STATE_DIRECTORY"] = true, t.TempDir() })
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	topic := feature.Chat{GroupID: "-100/7"}
	got := h.Message(ctx, feature.Message{Chat: topic, Sender: "a", Text: "/rain", Addressed: true})
	got.OnSent([]int64{40}, false)
	if _, ok := h.Press(ctx, feature.Press{Chat: feature.Chat{GroupID: "-200"}, Sent: 40, Data: "rerun"}); ok {
		t.Error("a press in another group was taken")
	}
	if a, ok := h.Press(ctx, feature.Press{Chat: feature.Chat{GroupID: "-100"}, Sent: 40, Data: "rerun"}); !ok || a != "" {
		t.Fatalf("refresh: %q %v", a, ok)
	}
	if p := h.Posts(); len(p) != 1 || p[0].Chat != topic || p[0].Text != `echo {"period":"24h"}` {
		t.Fatalf("posts %+v", p)
	}
}

// A feed menu's buttons name the topic they are in, so a press under an
// older message, which names the group alone, changes the topic's alerts.
// An item with an overlong ID is skipped, and a long link cut.
func TestFeedTopicAndLimits(t *testing.T) {
	old := feedEvery
	feedEvery = 20 * time.Millisecond
	t.Cleanup(func() { feedEvery = old })
	fake := &fakeMCP{feed: `{"kinds":[{"kind":"rain"},{"kind":"a/b"}],"items":[]}`}
	config := `{"mcpServers":{"a-server-name-that-is-rather-long-indeed-and-then-some-more":{"url":"` + serve(t, fake) + `","feed":{"resource":"feed://items"}}}}`
	h, _, err := register(t, config, func(h *featuretest.Host) {
		h.Bot.Caps.Buttons, h.Settings["STATE_DIRECTORY"], h.Shared = true, t.TempDir(), []feature.Group{{ID: "-100", Mod: true}} // a mod of the topic's group
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	topic := feature.Chat{GroupID: "-100/7"}
	var got feature.Action
	for range 200 {
		if got = h.Message(ctx, feature.Message{Chat: topic, Sender: "a", Text: "/alerts on", Addressed: true}); strings.Contains(got.Answer, "On: Rain") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if b := got.Markup.Buttons; len(b) != 1 || len(b[0]) != 2 || b[0][0].Data != "feed=alerts/open:alerts/7" || b[0][1].Data != "feed=alerts/open:sound/7" {
		t.Fatalf("buttons %+v", b)
	}
	bare := feature.Chat{GroupID: "-100"} // a press under an older message names the group alone
	if _, ok := h.Press(ctx, feature.Press{Chat: bare, Sent: 9, Data: got.Markup.Buttons[0][0].Data}); !ok {
		t.Fatal("press not taken")
	}
	if p := h.Posts(); len(p) != 1 || p[0].Chat != topic || p[0].Edit != 9 || p[0].Markup.Buttons[1][0].Data != "feed=alerts/k:rain/7" || !p[0].Markup.Buttons[1][0].Highlight {
		t.Fatalf("posts %+v", p)
	}
	if _, ok := h.Press(ctx, feature.Press{Chat: bare, Sent: 9, Data: "feed=alerts/off/7"}); !ok {
		t.Fatal("press not taken")
	}
	if a := h.Message(ctx, feature.Message{Chat: topic, Sender: "a", Text: "/alerts", Addressed: true}); !strings.Contains(a.Answer, "On: nothing") {
		t.Fatalf("the topic's alerts: %q", a.Answer)
	}
	h.Message(ctx, feature.Message{Chat: topic, Sender: "a", Text: "/alerts on", Addressed: true})
	now := time.Now().UTC().Format(time.RFC3339)
	fake.mu.Lock()
	fake.feed = `{"items":[{"id":"` + strings.Repeat("x", 201) + `","time":"` + now + `","text":"long id"},{"id":"2","time":"` + now + `","text":"ok","link":"https://x/` + strings.Repeat("y", 600) + `"}]}`
	fake.mu.Unlock()
	menus := len(h.Posts()) // the menus the presses redrew
	for range 200 {
		if len(h.Posts()) > menus {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(3 * feedEvery)
	p := h.Posts()
	if len(p) != menus+1 || !strings.HasPrefix(p[menus].Text, "ok\nhttps://x/") || utf8.RuneCountInString(p[menus].Text) > 3+maxLink+1 {
		t.Fatalf("posts %d, last %q", len(p), p[len(p)-1].Text)
	}
}

// A server that can't be reached adds no keys; a state directory that
// can't be used leaves direct commands without buttons and feeds off,
// but every server still starts.
func TestKeyboardDownAndNoState(t *testing.T) {
	url := serve(t, &fakeMCP{})
	config := `{"mcpServers":{
		"up":{"url":"` + url + `","keyboard":[["Up","/up"]],"feed":{"resource":"feed://items"},"chat_commands":{"up":{"tool":"echo","direct":true}}},
		"down":{"url":"http://127.0.0.1:1","keyboard":[["Down","/down"]]}}}`
	h, log, err := register(t, config, func(h *featuretest.Host) {
		h.Bot.Caps.Keyboard, h.Bot.Caps.Buttons, h.StoreErr = true, true, errors.New("no state directory")
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dm := func(text string) feature.Action {
		return h.Message(ctx, feature.Message{Chat: feature.Chat{User: "a"}, Sender: "a", Text: text, Addressed: true})
	}
	if k := dm("/keyboard").Markup.Keyboard; len(k) != 1 || len(k[0]) != 1 || k[0][0].Label != "Up" {
		t.Errorf("keyboard %+v", k)
	}
	if got := dm("/up"); got.Answer != "echo {}" || got.Markup.Buttons != nil {
		t.Errorf("/up %+v", got)
	}
	if got := dm("/alerts"); !got.IsZero() {
		t.Errorf("/alerts %+v", got)
	}
	if !strings.Contains(log.String(), "feeds are off") || !strings.Contains(log.String(), "direct commands and feed charts have no buttons") {
		t.Errorf("log %q", log.String())
	}
}

// Only an answer that goes out as one message gets buttons: not two
// pictures, nor a picture with more text than a caption takes. A free
// text argument is made again by refresh; a result that turns from text
// into a picture is a new post, leaving the old one and its buttons.
func TestRerunShapes(t *testing.T) {
	config := `{"mcpServers":{"fake":{"url":"` + serve(t, &fakeMCP{}) + `","allowed_tools":["echo","pic"],"chat_commands":{
		"say":{"tool":"echo","direct":true,"arg":"what"},
		"photo":{"tool":"pic","direct":true}}}}}`
	h, _, err := register(t, config, func(h *featuretest.Host) {
		h.Bot.Caps.Buttons, h.Bot.Caps.Caption, h.Settings["STATE_DIRECTORY"] = true, 1024, t.TempDir()
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	alice := feature.Chat{User: "alice"}
	say := h.Message(ctx, feature.Message{Chat: alice, Sender: "a", Text: "/say Sydney", Addressed: true})
	say.OnSent([]int64{5}, false)
	if _, ok := h.Press(ctx, feature.Press{Chat: alice, Sent: 5, Data: "rerun"}); !ok {
		t.Fatal("refresh not taken")
	}
	if p := h.Posts(); len(p) != 1 || p[0].Text != `echo {"what":"Sydney"}` || p[0].Edit != 5 {
		t.Fatalf("refresh of free text %+v", p)
	}
	photo := h.Message(ctx, feature.Message{Chat: alice, Sender: "a", Text: "/photo", Addressed: true})
	if len(photo.Pictures) != 1 || photo.Markup.Buttons == nil {
		t.Fatalf("one picture with a caption: %+v", photo.Markup)
	}

	d := &plainDirect{server: "x", again: &reruns{h: h}}
	for _, c := range []struct {
		act  feature.Action
		want bool
	}{
		{feature.Action{Answer: "hi"}, true},
		{feature.Action{Answer: strings.Repeat("é", 1024), Pictures: make([]feature.Picture, 1)}, true},
		{feature.Action{Answer: strings.Repeat("😀", 513), Pictures: make([]feature.Picture, 1)}, false}, // 1,026 UTF-16 units
		{feature.Action{Answer: "two", Pictures: make([]feature.Picture, 2)}, false},
	} {
		if got := d.again.oneMessage(c.act, feature.Chat{User: "a"}); got != c.want {
			t.Errorf("%d runes, %d pictures: %v", len([]rune(c.act.Answer)), len(c.act.Pictures), got)
		}
	}
}

// Every ID a feed still lists stays remembered, however many others pass;
// of the others, the newest maxSeen.
func TestTrimSeen(t *testing.T) {
	seen := []string{"keep"}
	for i := range maxSeen + 5 {
		seen = append(seen, fmt.Sprint(i))
	}
	got := trimSeen(seen, []feedItem{{ID: "keep"}, {ID: "0"}})
	if len(got) != maxSeen+2 || got[0] != "keep" || got[1] != "0" || got[2] != "5" || got[len(got)-1] != fmt.Sprint(maxSeen+4) {
		t.Fatalf("%d kept: %q ... %q", len(got), got[:3], got[len(got)-1])
	}
}

// A tool's text marked for the user is all the text a direct command
// posts, and parts for the assistant alone never are; a resource's JSON is
// data, left out, and its words posted.
func TestPlainDirectAudience(t *testing.T) {
	h, _, err := register(t, `{"mcpServers":{"fake":{"url":"`+serve(t, &fakeMCP{})+`","chat_commands":{
		"rain":{"tool":"worded","direct":true},
		"now":{"resource":"json://now","direct":true}}}}}`)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dm := func(text string) feature.Action {
		return h.Message(ctx, feature.Message{Chat: feature.Chat{User: "a"}, Sender: "a", Text: text, Addressed: true})
	}
	if got := dm("/rain"); got.Answer != "3 mm of rain" || len(got.Pictures) != 0 {
		t.Errorf("/rain: %q, %d pictures", got.Answer, len(got.Pictures))
	}
	if got := dm("/now"); got.Answer != "**All good**" {
		t.Errorf("/now: %q", got.Answer)
	}
}

// On a network with buttons, an alert's chart that names its arg and
// values has a button per value and 🔄; a press draws it again with the
// value and edits the post, the alert's words kept.
func TestFeedChartButtons(t *testing.T) {
	old := feedEvery
	feedEvery = 20 * time.Millisecond
	t.Cleanup(func() { feedEvery = old })
	fake := &fakeMCP{feed: `{"items":[]}`}
	h, _, err := register(t, `{"mcpServers":{"eco":{"url":"`+serve(t, fake)+`","feed":{"resource":"feed://items"}}}}`,
		func(h *featuretest.Host) {
			h.Bot.Caps.Buttons, h.Bot.Caps.Caption, h.Settings["STATE_DIRECTORY"] = true, 1024, t.TempDir()
		})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	alice := feature.Chat{User: "alice"}
	h.Message(ctx, feature.Message{Chat: alice, Sender: "a", Text: "/alerts on", Addressed: true})
	time.Sleep(3 * feedEvery)
	fake.mu.Lock()
	fake.feed = `{"items":[{"id":"1","time":"` + time.Now().UTC().Format(time.RFC3339) + `","text":"Heavy rain",` +
		`"chart":{"tool":"pic","arguments":{"period":"1d"},"arg":"period","values":["1d","7d"]}}]}`
	fake.mu.Unlock()
	var p []feature.Post
	for range 200 {
		if p = h.Posts(); len(p) > 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	alert := p[len(p)-1]
	var labels []string
	for _, row := range alert.Markup.Buttons {
		for _, b := range row {
			labels = append(labels, b.Label)
		}
	}
	if alert.Text != "Heavy rain" || len(alert.Pictures) != 1 || !slices.Equal(labels, []string{"• 1d", "7d", "🔄"}) || alert.OnSent == nil {
		t.Fatalf("alert %q, %d pictures, buttons %q", alert.Text, len(alert.Pictures), labels)
	}
	alert.OnSent([]int64{77}, false)
	if a, ok := h.Press(ctx, feature.Press{Chat: alice, Sent: 77, Data: "rerun=7d"}); !ok || a != "" {
		t.Fatalf("press %q %v", a, ok)
	}
	edit := h.Posts()[len(h.Posts())-1]
	labels = nil
	for _, row := range edit.Markup.Buttons {
		for _, b := range row {
			labels = append(labels, b.Label)
		}
	}
	if edit.Edit != 77 || edit.Text != "Heavy rain" || len(edit.Pictures) != 1 || !slices.Equal(labels, []string{"1d", "• 7d", "🔄"}) {
		t.Fatalf("edit %+v, buttons %q", edit, labels)
	}
}

// On a bot on two networks, each chat gets what its own can show: buttons
// and a keyboard on one, words alone on the other.
func TestPerNetworkCaps(t *testing.T) {
	fake := &fakeMCP{feed: `{"kinds":[{"kind":"rain","label":"Rain"}],"items":[]}`}
	tgChat, sigChat := feature.Chat{User: "123"}, feature.Chat{User: "alice"}
	nets := func(c feature.Chat) feature.Net {
		if c == tgChat {
			return feature.Net{Name: "telegram", Caps: feature.Caps{Buttons: true, Keyboard: true, Caption: 1024}}
		}
		return feature.Net{Name: "signal"}
	}
	config := `{"mcpServers":{"w":{"url":"` + serve(t, fake) + `","allowed_tools":["echo"],"feed":{"resource":"feed://items"},
		"keyboard":[["Rain","/rain"]],
		"chat_commands":{"rain":{"tool":"echo","direct":true,"arg":"period","values":["1d","7d"]}}}}}`
	h, _, err := register(t, config, func(h *featuretest.Host) {
		h.Bot.Caps = feature.Caps{Buttons: true, Keyboard: true, Caption: 1024} // what any network can do
		h.Bot.Networks = []string{"signal", "telegram"}
		h.Nets = nets
		h.Settings["STATE_DIRECTORY"] = t.TempDir()
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ask := func(c feature.Chat, text string) feature.Action {
		return h.Message(ctx, feature.Message{Chat: c, Sender: c.User, Text: text, Addressed: true})
	}
	if tg, sig := ask(tgChat, "/rain"), ask(sigChat, "/rain"); tg.Markup.Buttons == nil || sig.Markup.Buttons != nil || tg.Answer != sig.Answer {
		t.Errorf("/rain: telegram %+v, signal %+v", tg.Markup, sig.Markup)
	}
	if tg, sig := ask(tgChat, "/keyboard"), ask(sigChat, "/keyboard"); tg.Markup.Keyboard == nil || sig.Answer != "This chat app has no keyboard of buttons." {
		t.Errorf("/keyboard: telegram %+v, signal %q", tg.Markup, sig.Answer)
	}
	for range 200 { // the feed's first read, for its kinds
		if strings.Contains(ask(sigChat, "/alerts").Answer, "rain") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if tg, sig := ask(tgChat, "/alerts on"), ask(sigChat, "/alerts on"); tg.Markup.Buttons == nil || sig.Markup.Buttons != nil || !strings.Contains(sig.Answer, "/alerts on or off") {
		t.Errorf("/alerts: telegram %+v, signal %q", tg.Markup, sig.Answer)
	}
}

// With alerts off, as a group starts, the menu lists every kind as off,
// and 🔔 Alerts opens a toggle per kind: one turns alerts on for that kind
// alone, highlighted.
func TestFeedMenuWhileOff(t *testing.T) {
	fake := &fakeMCP{feed: `{"kinds":[{"kind":"rain","label":"Rain"},{"kind":"wind","label":"Wind"}],"items":[]}`}
	h, _, err := register(t, `{"mcpServers":{"eco":{"url":"`+serve(t, fake)+`","feed":{"resource":"feed://items"}}}}`,
		func(h *featuretest.Host) {
			h.Bot.Caps.Buttons, h.Settings["STATE_DIRECTORY"], h.Shared = true, t.TempDir(), []feature.Group{{ID: "g1", Admin: true}}
		})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	group := feature.Chat{GroupID: "g1"} // a group starts off
	var menu feature.Action
	for range 200 {
		if menu = h.Message(ctx, feature.Message{Chat: group, Sender: "alice", Text: "/alerts", Addressed: true}); strings.Contains(menu.Answer, "Rain") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	var labels []string
	for _, row := range menu.Markup.Buttons {
		for _, b := range row {
			labels = append(labels, b.Label+"="+b.Data)
		}
	}
	if !slices.Equal(labels, []string{"🔔 Alerts ▸=feed=alerts/open:alerts"}) || !strings.Contains(menu.Answer, "✅ On: nothing\n🔕 Off: Rain, Wind") {
		t.Fatalf("menu %q, buttons %q", menu.Answer, labels)
	}
	if _, ok := h.Press(ctx, feature.Press{Chat: group, Sent: 3, Data: "feed=alerts/open:alerts"}); !ok {
		t.Fatal("press not taken")
	}
	p := h.Posts()
	if b := p[0].Markup.Buttons; len(b) != 2 || b[0][0].Label != "🔔 Alerts ▾" || len(b[1]) != 2 || b[1][0].Highlight || b[1][1].Data != "feed=alerts/k:wind" {
		t.Fatalf("opened: %+v", b)
	}
	if _, ok := h.Press(ctx, feature.Press{Chat: group, Sent: 3, Data: "feed=alerts/k:wind"}); !ok {
		t.Fatal("press not taken")
	}
	p = h.Posts()
	if last := p[len(p)-1]; !strings.Contains(last.Text, "On: Wind\n🔕 Off: Rain") || !last.Markup.Buttons[1][1].Highlight || last.Markup.Buttons[1][0].Highlight {
		t.Fatalf("after choosing wind: %+v", last)
	}
}

// In a group, only those GROUP_ADMINS allows may change its alerts: by
// default its admins and mods, as the network lists them; anyone may see
// the menu and open it. A refused press tells the presser why and changes
// nothing. In a DM, its owner may, whatever GROUP_ADMINS says.
func TestFeedGroupAdmins(t *testing.T) {
	fake := &fakeMCP{feed: `{"kinds":[{"kind":"rain","label":"Rain"}],"items":[]}`}
	config := `{"mcpServers":{"eco":{"url":"` + serve(t, fake) + `","feed":{"resource":"feed://items"}}}}`
	ctx := context.Background()
	group, topic, alice := feature.Chat{GroupID: "g1"}, feature.Chat{GroupID: "g1/7"}, feature.Chat{User: "alice"}
	// start registers the feed with GROUP_ADMINS set to admins ("" unset)
	// and the sender a member of g1 as admin or mod, and waits for its
	// first read.
	start := func(admins string, admin, mod bool) *featuretest.Host {
		t.Helper()
		h, _, err := register(t, config, func(h *featuretest.Host) {
			h.Bot.Caps.Buttons, h.Settings["STATE_DIRECTORY"] = true, t.TempDir()
			if admins != "" {
				h.Settings["GROUP_ADMINS"] = admins
			}
			h.Shared = []feature.Group{{ID: "g1", Name: "Garden", Admin: admin, Mod: mod}}
		})
		if err != nil {
			t.Fatal(err)
		}
		for range 200 {
			if strings.Contains(h.Message(ctx, feature.Message{Chat: alice, Sender: "alice", Text: "/alerts", Addressed: true}).Answer, "Rain") {
				return h
			}
			time.Sleep(5 * time.Millisecond)
		}
		t.Fatal("the feed wasn't read")
		return nil
	}
	ask := func(h *featuretest.Host, chat feature.Chat, sender, text string) string {
		return h.Message(ctx, feature.Message{Chat: chat, Sender: sender, Text: text, Addressed: true}).Answer
	}
	const garden = "Only Garden's admins and mods can change its alerts."

	h := start("", false, false) // bob is neither
	for _, cmd := range []string{"/alerts on", "/alerts off", "/alerts sound on", "/alerts rain", "/alerts hail"} {
		if a := ask(h, group, "bob", cmd); a != garden {
			t.Errorf("%s: %q", cmd, a)
		}
	}
	if a := ask(h, group, "bob", "/alerts"); !strings.Contains(a, "On: nothing") {
		t.Errorf("the menu: %q", a)
	}
	if a, ok := h.Press(ctx, feature.Press{Chat: group, By: feature.Person{ID: "bob"}, Sent: 4, Data: "feed=alerts/open:alerts"}); !ok || a != "" || len(h.Posts()) != 1 {
		t.Errorf("opening the menu: %q %v %d", a, ok, len(h.Posts()))
	}
	for _, data := range []string{"feed=alerts/on", "feed=alerts/k:rain", "feed=alerts/soundon"} {
		if a, ok := h.Press(ctx, feature.Press{Chat: group, By: feature.Person{ID: "bob"}, Sent: 4, Data: data}); !ok || a != garden {
			t.Errorf("press %s: %q %v", data, a, ok)
		}
	}
	if n := len(h.Posts()); n != 1 {
		t.Errorf("a refused press redrew the menu: %d posts", n)
	}
	if a := ask(h, group, "bob", "/alerts"); !strings.Contains(a, "On: nothing") {
		t.Errorf("changed by a refused press: %q", a)
	}
	if a := ask(h, alice, "alice", "/alerts off"); !strings.Contains(a, "On: nothing") {
		t.Errorf("a DM's owner: %q", a)
	}

	h = start("", true, false) // alice is an admin, here in a forum topic
	if a := ask(h, topic, "alice", "/alerts on"); !strings.Contains(a, "On: Rain") {
		t.Errorf("an admin, in a topic: %q", a)
	}
	if a, ok := h.Press(ctx, feature.Press{Chat: group, By: feature.Person{ID: "alice"}, Sent: 5, Data: "feed=alerts/k:rain/7"}); !ok || a != "" {
		t.Errorf("an admin's press: %q %v", a, ok)
	}
	if a := ask(h, topic, "alice", "/alerts"); !strings.Contains(a, "Off: Rain") {
		t.Errorf("after an admin's press: %q", a)
	}

	h = start("mods", true, false) // only mods: alice, an admin, isn't one
	if a := ask(h, group, "alice", "/alerts on"); a != "Only Garden's mods can change its alerts." {
		t.Errorf("mods only, an admin: %q", a)
	}
	h = start("mods", false, true)
	if a := ask(h, group, "alice", "/alerts on"); !strings.Contains(a, "On: Rain") {
		t.Errorf("mods only, a mod: %q", a)
	}

	h = start("none, signal:bob", false, false) // bob is listed; alice isn't, and no role may
	if a := ask(h, group, "bob", "/alerts on"); !strings.Contains(a, "On: Rain") {
		t.Errorf("listed: %q", a)
	}
	if a := ask(h, group, "alice", "/alerts off"); a != "Changing this group's alerts isn't open to you." {
		t.Errorf("not listed: %q", a)
	}
	if a := ask(h, alice, "alice", "/alerts off"); !strings.Contains(a, "On: nothing") {
		t.Errorf("a DM's owner, not listed: %q", a)
	}

	if n := h.GroupsAsked(); n != 0 {
		t.Errorf("no role may, yet the network was asked %d times", n)
	}

	h = start("admins, bob", true, false) // bob listed, alice by role
	if a := ask(h, group, "bob", "/alerts on"); !strings.Contains(a, "On: Rain") || h.GroupsAsked() != 0 {
		t.Errorf("listed, with a role named: %q, asked %d", a, h.GroupsAsked())
	}
	if a := ask(h, group, "alice", "/alerts"); !strings.Contains(a, "On: Rain") || h.GroupsAsked() != 0 {
		t.Errorf("the menu asked the network: %q, asked %d", a, h.GroupsAsked())
	}
	if _, ok := h.Press(ctx, feature.Press{Chat: group, By: feature.Person{ID: "alice"}, Sent: 4, Data: "feed=alerts/open:alerts"}); !ok || h.GroupsAsked() != 0 {
		t.Errorf("opening the menu asked the network: %d", h.GroupsAsked())
	}
	if a := ask(h, group, "alice", "/alerts off"); !strings.Contains(a, "On: nothing") || h.GroupsAsked() != 1 {
		t.Errorf("an admin, not listed: %q, asked %d", a, h.GroupsAsked())
	}
	h.GroupsErr = errors.New("down") // the network can't say: refused
	if a := ask(h, group, "alice", "/alerts on"); a != "Only this group's admins can change its alerts." {
		t.Errorf("the network failing: %q", a)
	}
	if a, ok := h.Press(ctx, feature.Press{Chat: group, By: feature.Person{ID: "alice"}, Sent: 4, Data: "feed=alerts/on"}); !ok || a != "Only this group's admins can change its alerts." {
		t.Errorf("a press, the network failing: %q %v", a, ok)
	}
	if a := ask(h, group, "bob", "/alerts on"); !strings.Contains(a, "On: Rain") {
		t.Errorf("listed, the network failing: %q", a)
	}

	h = start("all", false, false)
	if a := ask(h, group, "bob", "/alerts on"); !strings.Contains(a, "On: Rain") || h.GroupsAsked() != 0 {
		t.Errorf("all: %q, asked %d", a, h.GroupsAsked())
	}
}

// Alerts are on by default in a DM, from its first message to the bot:
// every kind, silently. One turned off stays off; a group starts off.
func TestFeedOnByDefaultInDMs(t *testing.T) {
	old := feedEvery
	feedEvery = 20 * time.Millisecond
	t.Cleanup(func() { feedEvery = old })
	fake := &fakeMCP{feed: `{"kinds":[{"kind":"rain","label":"Rain"}],"items":[]}`}
	dir := t.TempDir()
	config := `{"mcpServers":{"eco":{"url":"` + serve(t, fake) + `","feed":{"resource":"feed://items"}}}}`
	h, _, err := register(t, config, func(h *featuretest.Host) { h.Settings["STATE_DIRECTORY"] = dir })
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	say := func(c feature.Chat, text string) string {
		return h.Message(ctx, feature.Message{Chat: c, Sender: "alice", Text: text, Addressed: true}).Answer
	}
	alice, bob, group := feature.Chat{User: "alice"}, feature.Chat{User: "bob"}, feature.Chat{GroupID: "g1"}
	say(alice, "hello")
	say(group, "hello")
	isOn := func(a string) bool {
		return strings.Contains(a, "✅ On: Rain") || strings.Contains(a, "chat: on, silently")
	}
	isOff := func(a string) bool { return strings.Contains(a, "✅ On: nothing") || strings.Contains(a, "chat: off") }
	if a := say(alice, "/alerts"); !isOn(a) {
		t.Errorf("a DM after its first message: %q", a)
	}
	if a := say(bob, "/alerts"); !isOn(a) {
		t.Errorf("a DM's first message being /alerts: %q", a)
	}
	if a := say(group, "/alerts"); !isOff(a) {
		t.Errorf("a group: %q", a)
	}
	say(alice, "/alerts off")
	say(alice, "hello again")
	h.Stop()
	h, _, err = register(t, config, func(h *featuretest.Host) { h.Settings["STATE_DIRECTORY"] = dir }) // restarted
	if err != nil {
		t.Fatal(err)
	}
	say(alice, "and again")
	if a := say(alice, "/alerts"); !isOff(a) {
		t.Errorf("a DM that turned alerts off, after a restart: %q", a)
	}
	if a := say(alice, "/alerts on"); !isOn(a) {
		t.Errorf("turned on again: %q", a)
	}
}

// In a DM, 🔊 Sound opens a toggle per kind that is on, and replaces
// 🔔 Alerts while open; a kind with sound notifies, the others are silent.
func TestFeedSoundPerKind(t *testing.T) {
	old := feedEvery
	feedEvery = 20 * time.Millisecond
	t.Cleanup(func() { feedEvery = old })
	fake := &fakeMCP{feed: `{"kinds":[{"kind":"rain","label":"Rain"},{"kind":"wind","label":"Wind"},{"kind":"uv","label":"UV"}],"items":[]}`}
	h, _, err := register(t, `{"mcpServers":{"eco":{"url":"`+serve(t, fake)+`","feed":{"resource":"feed://items"}}}}`,
		func(h *featuretest.Host) { h.Bot.Caps.Buttons, h.Settings["STATE_DIRECTORY"] = true, t.TempDir() })
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	alice := feature.Chat{User: "alice"}
	var menu feature.Action
	for range 200 {
		if menu = h.Message(ctx, feature.Message{Chat: alice, Sender: "alice", Text: "/alerts uv", Addressed: true}); strings.Contains(menu.Answer, "UV") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if b := menu.Markup.Buttons; len(b) != 1 || len(b[0]) != 2 || b[0][0].Label != "🔔 Alerts ▸" || b[0][1].Label != "🔊 Sound ▸" {
		t.Fatalf("closed: %+v", b)
	}
	press := func(data string) feature.Post {
		t.Helper()
		if a, ok := h.Press(ctx, feature.Press{Chat: alice, Sent: 5, Data: data}); !ok || a != "" {
			t.Fatalf("%s: %q %v", data, a, ok)
		}
		p := h.Posts()
		return p[len(p)-1]
	}
	p := press("feed=alerts/open:sound")
	var labels []string
	for _, row := range p.Markup.Buttons {
		for _, b := range row {
			labels = append(labels, b.Label)
		}
	}
	if !slices.Equal(labels, []string{"🔊 Sound ▾", "R̶a̶i̶n̶", "W̶i̶n̶d̶"}) { // UV is off, so has no sound
		t.Fatalf("sound menu %q", labels)
	}
	p = press("feed=alerts/s:rain")
	if b := p.Markup.Buttons; b[0][0].Label != "🔊 Sound ▾" || b[1][0].Label != "Rain" || !b[1][0].Highlight || !strings.Contains(p.Text, "🔊 With sound: Rain") {
		t.Fatalf("after rain's sound: %q %+v", p.Text, b)
	}
	if p = press("feed=alerts/close"); len(p.Markup.Buttons) != 1 || len(p.Markup.Buttons[0]) != 2 {
		t.Fatalf("closed again: %+v", p.Markup.Buttons)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	fake.mu.Lock()
	fake.feed = `{"kinds":[{"kind":"rain"},{"kind":"wind"},{"kind":"uv"}],"items":[{"id":"1","kind":"rain","time":"` + now + `","text":"Rain"},{"id":"2","kind":"wind","time":"` + now + `","text":"Wind"}]}`
	fake.mu.Unlock()
	before := len(h.Posts())
	for range 200 {
		if len(h.Posts()) >= before+2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := map[string]bool{}
	for _, p := range h.Posts()[before:] {
		got[p.Text] = p.Markup.Silent
	}
	if silent, ok := got["Rain"]; !ok || silent {
		t.Errorf("rain alert: %v", got)
	}
	if silent, ok := got["Wind"]; !ok || !silent {
		t.Errorf("wind alert: %v", got)
	}
}

// Sound for every kind, with one kind then turned off, keeps sound for the
// kinds the feed doesn't list; a menu shows every kind, however many; and
// with no kinds known, the menus toggle all alerts, and all sound.
func TestFeedMenuEdges(t *testing.T) {
	s := &feedSub{Sound: true}
	f := &feed{kinds: []feedKind{{Kind: "rain"}, {Kind: "wind"}}}
	fs := &feeds{doc: map[string]*feedState{}, store: nopStore{}}
	fs.state("x").Chats["u:a"] = s
	f.server = "x"
	fs.set(f, feature.Chat{User: "a"}, "s:wind")
	if s.loud("wind") || !s.loud("rain") || !s.loud("storm") || !s.loud("") {
		t.Errorf("after wind's sound off: %+v", s)
	}
	fs.set(f, feature.Chat{User: "a"}, "s:wind")
	if !s.loud("wind") {
		t.Errorf("wind's sound back on: %+v", s)
	}

	h := featuretest.New()
	h.Bot.Caps.Buttons = true
	fs.h = h
	var many []feedKind
	for i := range 20 {
		many = append(many, feedKind{Kind: fmt.Sprintf("k%d", i)})
	}
	big := &feed{server: "x", command: "alerts", kinds: many, title: "W"}
	_, rows := fs.menu(big, feature.Chat{User: "a"}, menuAlerts)
	n := 0
	for _, r := range rows[1:] {
		n += len(r)
	}
	if n != 20 {
		t.Errorf("%d toggles for 20 kinds", n)
	}

	none := &feed{server: "y", command: "alerts", title: "W"}
	_, rows = fs.menu(none, feature.Chat{GroupID: "g"}, menuAlerts)
	if len(rows) != 2 || rows[1][0].Data != "feed=alerts/on" || rows[1][0].Highlight {
		t.Fatalf("no kinds, off: %+v", rows)
	}
	fs.set(none, feature.Chat{GroupID: "g"}, "on")
	_, rows = fs.menu(none, feature.Chat{GroupID: "g"}, menuSound)
	if len(rows) != 2 || rows[0][0].Label != "🔊 Sound ▾" || rows[1][0].Data != "feed=alerts/soundon" {
		t.Fatalf("no kinds, on, sound: %+v", rows)
	}
}

// Turning all alerts on is saved: in a group, and in a DM that had turned
// them off.
func TestFeedOnIsSaved(t *testing.T) {
	f := &feed{server: "x", kinds: []feedKind{{Kind: "rain"}}}
	for _, chat := range []feature.Chat{{GroupID: "g"}, {User: "a"}} {
		st := &countStore{}
		fs := &feeds{doc: map[string]*feedState{}, store: st}
		fs.set(f, chat, "off")
		saves := st.n
		fs.set(f, chat, "on")
		if st.n != saves+1 {
			t.Errorf("%+v: on saved %d times", chat, st.n-saves)
		}
		if s := fs.state("x"); s.Chats[chat.Key()] == nil || s.Declined[chat.Key()] {
			t.Errorf("%+v: not on: %+v", chat, s)
		}
	}
}

// countStore keeps nothing, counting saves.
type countStore struct{ n int }

func (*countStore) Load(any) (bool, error) { return false, nil }
func (c *countStore) Save(any) error       { c.n++; return nil }

// nopStore keeps nothing.
type nopStore struct{}

func (nopStore) Load(any) (bool, error) { return false, nil }
func (nopStore) Save(any) error         { return nil }

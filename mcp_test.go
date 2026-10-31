package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/robdevops/signalbot/internal/kit/mcp"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeConfig(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "mcp.json")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadMCPConfig(t *testing.T) {
	good := `{"mcpServers":{"w":{"url":"https://x/mcp","headers":{"A":"b"},"allowed_tools":["t"]},"l":{"command":"x","args":["y"],"blocked_tools":["u"]}}}`
	s, err := loadMCPConfig(writeConfig(t, good))
	if err != nil || s["w"].URL != "https://x/mcp" || s["l"].Command != "x" || s["w"].Headers["A"] != "b" ||
		len(s["w"].AllowedTools) != 1 || s["w"].BlockedTools != nil || s["l"].AllowedTools != nil || len(s["l"].BlockedTools) != 1 {
		t.Fatalf("got %+v, %v", s, err)
	}
	for name, bad := range map[string]string{
		"both":     `{"mcpServers":{"w":{"url":"u","command":"c"}}}`,
		"neither":  `{"mcpServers":{"w":{"allowed_tools":["t"]}}}`,
		"not json": `{`,
	} {
		if _, err := loadMCPConfig(writeConfig(t, bad)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	// A disabled server is dropped unchecked, even if it is incomplete.
	s, err = loadMCPConfig(writeConfig(t, `{"mcpServers":{"on":{"url":"u"},"off":{"disabled":true},"off2":{"url":"u","disabled":true}}}`))
	if err != nil || len(s) != 1 || s["on"].URL != "u" {
		t.Fatalf("disabled servers: %+v, %v", s, err)
	}
	if s, err := loadMCPConfig(writeConfig(t, `{"mcpServers":{"on":{"url":"u","disabled":false}}}`)); err != nil || len(s) != 1 {
		t.Fatalf("disabled false: %+v, %v", s, err)
	}
	if _, err := loadMCPConfig(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("a missing file must be an error")
	}
}

func TestToolModelName(t *testing.T) {
	if got := toolModelName("my.server", "get forecast"); got != "my_server__get_forecast" {
		t.Errorf("got %q", got)
	}
	if got := toolModelName(strings.Repeat("s", 50), strings.Repeat("t", 50)); len(got) != 64 {
		t.Errorf("not capped at 64: %d", len(got))
	}
}

// fakeMCP is a Streamable HTTP MCP server with three tools: echo returns its
// arguments, fail reports an error, and pic returns an image.
type fakeMCP struct {
	sse bool

	mu       sync.Mutex
	session  string // the live session id
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
		f.inits++
		f.session = fmt.Sprintf("s%d", f.inits)
		w.Header().Set("Mcp-Session-Id", f.session)
	} else if r.Header.Get("Mcp-Session-Id") != f.session {
		f.badCalls++
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	} else if r.Header.Get("MCP-Protocol-Version") != mcp.Protocol {
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
		result = `{"protocolVersion":"` + mcp.Protocol + `","capabilities":{"tools":{}},"serverInfo":{"name":"fake","version":"1"}}`
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

func startFake(t *testing.T, f *fakeMCP, tools ...string) (*MCP, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	m, err := StartMCP(context.Background(), map[string]mcpServerConfig{"fake": {Server: mcp.Server{URL: srv.URL}, AllowedTools: tools}})
	if err != nil {
		t.Fatal(err)
	}
	return m, srv
}

func TestHTTPMCPTools(t *testing.T) {
	for _, sse := range []bool{false, true} {
		f := &fakeMCP{sse: sse}
		m, _ := startFake(t, f, "echo", "fail", "pic")
		if got := strings.Join(m.Names(), ","); got != "echo,fail,pic" {
			t.Fatalf("sse=%v: tools %q (the second tools/list page must be read)", sse, got)
		}
		out, _, tool, err := m.Call(context.Background(), "fake__echo", `{"q":"weather"}`)
		if err != nil || out != `echo {"q":"weather"}` || tool.Name != "echo" || tool.Server != "fake" {
			t.Fatalf("sse=%v: echo = %q, %+v, %v", sse, out, tool, err)
		}
		if out, _, _, err := m.Call(context.Background(), "fake__fail", ""); err == nil || out != "error: it broke" {
			t.Errorf("sse=%v: fail = %q, %v", sse, out, err)
		}
		if out, imgs, _, _ := m.Call(context.Background(), "fake__pic", "{}"); out != "[non-text content omitted]\n[image sent to the chat]\ncaption" || len(imgs) != 1 {
			t.Errorf("sse=%v: pic = %q, %d images", sse, out, len(imgs))
		}
		if out, _, _, err := m.Call(context.Background(), "fake__nope", "{}"); err == nil || !strings.Contains(out, "no tool") {
			t.Errorf("sse=%v: unknown tool = %q, %v", sse, out, err)
		}
		if out, _, _, err := m.Call(context.Background(), "fake__echo", "not json"); err == nil || !strings.Contains(out, "JSON object") {
			t.Errorf("sse=%v: bad arguments = %q, %v", sse, out, err)
		}
		if f.badCalls != 0 {
			t.Errorf("sse=%v: %d requests without the session id", sse, f.badCalls)
		}
	}
}

// allowed_tools is a whitelist and blocked_tools a blacklist; without
// either, every tool is offered. An empty allowed_tools offers none.
func TestMCPToolLists(t *testing.T) {
	for _, tc := range []struct {
		name             string
		allowed, blocked []string
		want             string
	}{
		{"neither", nil, nil, "echo,fail,pic"},
		{"whitelist", []string{"echo"}, nil, "echo"},
		{"empty whitelist", []string{}, nil, ""},
		{"blacklist", nil, []string{"fail"}, "echo,pic"},
		{"both", []string{"echo", "fail"}, []string{"fail"}, "echo"},
	} {
		srv := httptest.NewServer(&fakeMCP{})
		m, err := StartMCP(context.Background(), map[string]mcpServerConfig{"fake": {Server: mcp.Server{URL: srv.URL}, AllowedTools: tc.allowed, BlockedTools: tc.blocked}})
		srv.Close()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := strings.Join(m.Names(), ","); got != tc.want {
			t.Errorf("%s: tools %q, want %q", tc.name, got, tc.want)
		}
	}
	m, _ := startFake(t, &fakeMCP{}, "echo")
	if _, _, _, err := m.Call(context.Background(), "fake__fail", "{}"); err == nil {
		t.Fatal("a tool outside the whitelist was callable")
	}
}

// When the server forgets the session, the client starts a new one, once.
func TestHTTPMCPSessionExpiry(t *testing.T) {
	f := &fakeMCP{}
	m, _ := startFake(t, f, "echo")
	f.mu.Lock()
	f.session = "rotated"
	f.mu.Unlock()
	if out, _, _, err := m.Call(context.Background(), "fake__echo", "{}"); err != nil || out != "echo {}" {
		t.Fatalf("after expiry: %q, %v", out, err)
	}
	if f.inits != 2 {
		t.Fatalf("%d initializations, want 2", f.inits)
	}
}

func TestMCPStartupErrors(t *testing.T) {
	srv := httptest.NewServer(&fakeMCP{})
	defer srv.Close()
	if _, err := StartMCP(context.Background(), map[string]mcpServerConfig{"fake": {Server: mcp.Server{URL: srv.URL}, AllowedTools: []string{"echo", "typo"}}}); err == nil {
		t.Error("a whitelisted tool the server lacks must be an error")
	}
	if _, err := StartMCP(context.Background(), map[string]mcpServerConfig{"fake": {Server: mcp.Server{URL: srv.URL}, BlockedTools: []string{"typo"}}}); err == nil {
		t.Error("a blacklisted tool the server lacks must be an error, or a typo would leave the real tool offered")
	}
	srv2 := httptest.NewServer(&fakeMCP{})
	defer srv2.Close()
	if _, err := StartMCP(context.Background(), map[string]mcpServerConfig{
		"a.b": {Server: mcp.Server{URL: srv.URL}, AllowedTools: []string{"echo"}},
		"a_b": {Server: mcp.Server{URL: srv2.URL}, AllowedTools: []string{"echo"}},
	}); err == nil {
		t.Error("two tools with the same model name must be an error")
	}
	// An unreachable server is left out, and the bot still starts.
	srv2.Close()
	m, err := StartMCP(context.Background(), map[string]mcpServerConfig{"gone": {Server: mcp.Server{URL: srv2.URL}, AllowedTools: []string{"echo"}}})
	if err != nil || len(m.Names()) != 0 {
		t.Fatalf("unreachable server: %v, %v", m.Names(), err)
	}
}

// TestMCPHelperProcess is not a real test: run as a subprocess with
// MCP_HELPER=1, it is a stdio MCP server with two tools. env reports what the
// process can see; exit makes it quit after answering.
func TestMCPHelperProcess(t *testing.T) {
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
			result = `{"protocolVersion":"` + mcp.Protocol + `","capabilities":{"tools":{}},"serverInfo":{"name":"helper","version":"1"}}`
		case "tools/list":
			result = `{"tools":[{"name":"env"},{"name":"exit"}]}`
		case "tools/call":
			text, _ := json.Marshal("key=" + os.Getenv("OPENROUTER_API_KEY") + " marker=" + os.Getenv("MCP_HELPER"))
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
// key, and is restarted when it exits.
func TestStdioMCP(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "secret-key")
	ctx, cancel := context.WithCancel(context.Background())
	m, err := StartMCP(ctx, map[string]mcpServerConfig{"local": {
		Server: mcp.Server{Command: os.Args[0], Args: []string{"-test.run=^TestMCPHelperProcess$"},
			Env: map[string]string{"MCP_HELPER": "1"}},
		AllowedTools: []string{"env", "exit"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); m.Wait() }()

	if out, _, _, err := m.Call(ctx, "local__env", "{}"); err != nil || out != "key= marker=1" {
		t.Fatalf("env = %q, %v", out, err)
	}
	m.Call(ctx, "local__exit", "{}")
	deadline := time.Now().Add(10 * time.Second)
	for {
		out, _, _, err := m.Call(ctx, "local__env", "{}")
		if err == nil && out == "key= marker=1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("not restarted: %q, %v", out, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestMCPByServer(t *testing.T) {
	a, b := httptest.NewServer(&fakeMCP{}), httptest.NewServer(&fakeMCP{})
	defer a.Close()
	defer b.Close()
	m, err := StartMCP(context.Background(), map[string]mcpServerConfig{
		"yahoo": {Server: mcp.Server{URL: a.URL}, AllowedTools: []string{"echo", "pic"}},
		"scout": {Server: mcp.Server{URL: b.URL}, AllowedTools: []string{"fail"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := m.ByServer(), "scout: fail; yahoo: echo, pic"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if (*MCP)(nil).ByServer() != "" {
		t.Fatal("no MCP must give no list")
	}
}

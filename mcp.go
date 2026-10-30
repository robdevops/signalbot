package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	mcpProtocol    = "2025-06-18"
	mcpCallTimeout = 30 * time.Second
	// maxToolResultRunes caps what one tool call adds to the conversation.
	maxToolResultRunes = 16 << 10
)

// mcpServerConfig is one entry of MCP_CONFIG's "mcpServers": a URL for a
// remote server, or a command for a local one. allowed_tools, when present,
// offers the model only those tools; blocked_tools, when present, offers all
// but those. Without either, every tool the server lists is offered.
type mcpServerConfig struct {
	URL          string            `json:"url"`
	Headers      map[string]string `json:"headers"`
	Command      string            `json:"command"`
	Args         []string          `json:"args"`
	Env          map[string]string `json:"env"`
	AllowedTools []string          `json:"allowed_tools"` // nil: not in whitelist mode
	BlockedTools []string          `json:"blocked_tools"`
	Disabled     bool              `json:"disabled"` // left out entirely
}

func loadMCPConfig(path string) (map[string]mcpServerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f struct {
		Servers map[string]mcpServerConfig `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	for name, s := range f.Servers {
		if s.Disabled {
			delete(f.Servers, name) // not started, and not checked, so it can be parked half-written
			continue
		}
		if (s.URL == "") == (s.Command == "") {
			return nil, fmt.Errorf("MCP server %q needs a url or a command, not both or neither", name)
		}
	}
	return f.Servers, nil
}

// mcpConn is a connection to one MCP server.
type mcpConn interface {
	call(ctx context.Context, method string, params any) (json.RawMessage, error)
	notify(ctx context.Context, method string, params any) error
}

// rpcError is a JSON-RPC error from an MCP server.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

type mcpMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// mcpInitialize performs the MCP handshake.
func mcpInitialize(ctx context.Context, c mcpConn) error {
	if _, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": mcpProtocol,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "signalbot", "version": buildVersion()},
	}); err != nil {
		return err
	}
	return c.notify(ctx, "notifications/initialized", nil)
}

// mcpTool is a tool the model may call.
type mcpTool struct {
	Model       string // the name the model sees: server__tool
	Server      string
	Name        string // the server's name for it
	Description string
	Schema      json.RawMessage
}

// MCP holds the tools of every configured server that the model may call.
type MCP struct {
	tools   []mcpTool // sorted by Model
	byModel map[string]mcpTool
	conns   map[string]mcpConn
	wg      sync.WaitGroup // stdio supervisors
}

var unsafeToolChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// toolModelName is the name the model sees for a server's tool.
func toolModelName(server, tool string) string {
	n := unsafeToolChars.ReplaceAllString(server, "_") + "__" + unsafeToolChars.ReplaceAllString(tool, "_")
	return n[:min(len(n), 64)]
}

// StartMCP connects to every server and collects the tools its
// allowed_tools and blocked_tools let through. A server that cannot be
// reached is logged and left out. A tool named in either list that a
// reachable server does not have, or two tools that end up with the same
// name, is an error: a typo in blocked_tools would otherwise leave the tool
// offered. Stdio servers are stopped when ctx ends.
func StartMCP(ctx context.Context, servers map[string]mcpServerConfig) (*MCP, error) {
	m := &MCP{byModel: map[string]mcpTool{}, conns: map[string]mcpConn{}}
	names := make([]string, 0, len(servers))
	for n := range servers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		cfg := servers[name]
		var conn mcpConn
		if cfg.URL != "" {
			conn = &httpConn{name: name, url: cfg.URL, headers: cfg.Headers, client: newHTTPClient()}
		} else {
			conn = &stdioConn{name: name, cfg: cfg}
		}
		listed, err := startServer(ctx, conn)
		if err != nil {
			errorf("MCP server %s unavailable, its tools are left out: %v", name, err)
			continue
		}
		for _, want := range append(slices.Clone(cfg.AllowedTools), cfg.BlockedTools...) {
			if _, ok := listed[want]; !ok {
				return nil, fmt.Errorf("MCP server %q has no tool %q", name, want)
			}
		}
		for _, tn := range slices.Sorted(maps.Keys(listed)) {
			if cfg.AllowedTools != nil && !slices.Contains(cfg.AllowedTools, tn) || slices.Contains(cfg.BlockedTools, tn) {
				continue
			}
			t := listed[tn]
			t.Server, t.Model = name, toolModelName(name, tn)
			if _, dup := m.byModel[t.Model]; dup {
				return nil, fmt.Errorf("MCP tool %q of server %q clashes with another tool's name", tn, name)
			}
			m.byModel[t.Model] = t
			m.tools = append(m.tools, t)
		}
		m.conns[name] = conn
		if s, ok := conn.(*stdioConn); ok {
			m.wg.Add(1)
			go func() { defer m.wg.Done(); s.supervise(ctx) }()
		}
	}
	sort.Slice(m.tools, func(i, j int) bool { return m.tools[i].Model < m.tools[j].Model })
	return m, nil
}

// startServer connects, initializes and lists the server's tools by name.
func startServer(ctx context.Context, conn mcpConn) (map[string]mcpTool, error) {
	ctx2, cancel := context.WithTimeout(ctx, mcpCallTimeout)
	defer cancel()
	if s, ok := conn.(*stdioConn); ok {
		if err := s.start(ctx); err != nil {
			return nil, err
		}
	}
	if err := mcpInitialize(ctx2, conn); err != nil {
		return nil, err
	}
	tools := map[string]mcpTool{}
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, err := conn.call(ctx2, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var page struct {
			Tools []struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				InputSchema json.RawMessage `json:"inputSchema"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(res, &page); err != nil {
			return nil, fmt.Errorf("tools/list: %v", err)
		}
		for _, t := range page.Tools {
			tools[t.Name] = mcpTool{Name: t.Name, Description: t.Description, Schema: t.InputSchema}
		}
		if cursor = page.NextCursor; cursor == "" {
			return tools, nil
		}
	}
}

// Names lists the tools the model may call by their servers' names, for {tools}.
func (m *MCP) Names() []string {
	if m == nil {
		return nil
	}
	var out []string
	for _, t := range m.tools {
		out = append(out, t.Name)
	}
	return out
}

// ByServer lists the tools the model may call, grouped by server, as
// "server: tool, tool; server: tool", for the system prompt.
func (m *MCP) ByServer() string {
	if m == nil {
		return ""
	}
	var groups []string
	for i, t := range m.tools { // sorted by server__tool, so grouped by server
		if i == 0 || m.tools[i-1].Server != t.Server {
			groups = append(groups, t.Server+": "+t.Name)
		} else {
			groups[len(groups)-1] += ", " + t.Name
		}
	}
	return strings.Join(groups, "; ")
}

// Wait waits for stdio servers to stop after the context given to StartMCP ends.
func (m *MCP) Wait() {
	if m != nil {
		m.wg.Wait()
	}
}

// Call runs the tool the model named, with the JSON arguments it wrote, and
// returns the text to give back to the model. err is for the log only, and
// never holds the arguments or the result.
func (m *MCP) Call(ctx context.Context, model, arguments string) (result string, t mcpTool, err error) {
	t, ok := m.byModel[model]
	if !ok {
		return "error: there is no tool named " + model, t, errors.New("unknown tool")
	}
	args := map[string]any{}
	if strings.TrimSpace(arguments) != "" {
		if json.Unmarshal([]byte(arguments), &args) != nil {
			return "error: the arguments were not a JSON object", t, errors.New("invalid arguments")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, mcpCallTimeout)
	defer cancel()
	res, err := m.conns[t.Server].call(ctx, "tools/call", map[string]any{"name": t.Name, "arguments": args})
	if err != nil {
		var re *rpcError
		if errors.As(err, &re) {
			return "error: " + clamp(re.Message, 500), t, fmt.Errorf("rpc error %d", re.Code)
		}
		return "error: the tool could not be reached", t, err
	}
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if json.Unmarshal(res, &r) != nil {
		return "error: the tool sent a malformed result", t, errors.New("malformed result")
	}
	var parts []string
	for _, c := range r.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		} else {
			parts = append(parts, "[non-text content omitted]")
		}
	}
	out := clamp(strings.Join(parts, "\n"), maxToolResultRunes)
	if r.IsError {
		return "error: " + out, t, errors.New("the tool reported an error")
	}
	return out, t, nil
}

// --- stdio --------------------------------------------------------------

// stdioConn runs a local MCP server and talks to it over stdin and stdout,
// one JSON message per line.
type stdioConn struct {
	name string
	cfg  mcpServerConfig

	nextID  atomic.Int64
	mu      sync.Mutex // guards stdin, pending and done
	stdin   io.WriteCloser
	pending map[int64]chan mcpMsg
	done    chan struct{} // closed when the current process exits
}

// stdioEnv is the server's whole environment: PATH, HOME and its configured
// env, never the bot's own, which holds the API key.
func stdioEnv(extra map[string]string) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

func (c *stdioConn) start(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, c.cfg.Command, c.cfg.Args...)
	cmd.Env = stdioEnv(c.cfg.Env)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	c.mu.Lock()
	c.stdin, c.pending, c.done = stdin, map[int64]chan mcpMsg{}, done
	c.mu.Unlock()

	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			debugf("MCP server %s: %s", c.name, sc.Text())
		}
	}()
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 64<<10), 4<<20)
		for sc.Scan() {
			c.dispatch(sc.Bytes())
		}
		cmd.Wait()
		c.mu.Lock()
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.stdin = nil
		c.mu.Unlock()
		close(done)
	}()
	return nil
}

// supervise restarts the server whenever it exits, until ctx ends.
func (c *stdioConn) supervise(ctx context.Context) {
	backoff := time.Second
	for {
		c.mu.Lock()
		done := c.done
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			<-done
			return
		case <-done:
		}
		warnf("MCP server %s exited; restarting in %s", c.name, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 32*time.Second)
		if err := c.start(ctx); err != nil {
			warnf("MCP server %s failed to start: %v", c.name, err)
			c.mu.Lock()
			c.done = closedChan()
			c.mu.Unlock()
			continue
		}
		ictx, cancel := context.WithTimeout(ctx, mcpCallTimeout)
		if err := mcpInitialize(ictx, c); err != nil {
			warnf("MCP server %s failed to initialize: %v", c.name, err)
		} else {
			backoff = time.Second
		}
		cancel()
	}
}

func closedChan() chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func (c *stdioConn) dispatch(line []byte) {
	var m mcpMsg
	if json.Unmarshal(line, &m) != nil {
		warnf("bad JSON from MCP server %s (%d bytes), ignored", c.name, len(line))
		return
	}
	switch {
	case m.ID != nil && m.Method != "":
		// A request from the server: answer pings, refuse the rest.
		reply := mcpMsg{JSONRPC: "2.0", ID: m.ID, Result: json.RawMessage(`{}`)}
		if m.Method != "ping" {
			reply = mcpMsg{JSONRPC: "2.0", ID: m.ID, Error: &rpcError{Code: -32601, Message: "method not supported"}}
		}
		c.write(reply)
	case m.ID != nil:
		c.mu.Lock()
		ch := c.pending[*m.ID]
		delete(c.pending, *m.ID)
		c.mu.Unlock()
		if ch != nil {
			ch <- m
		}
	}
}

func (c *stdioConn) write(m mcpMsg) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stdin == nil {
		return errors.New("not running")
	}
	_, err = c.stdin.Write(append(data, '\n'))
	return err
}

func (c *stdioConn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	ch := make(chan mcpMsg, 1)
	c.mu.Lock()
	if c.stdin == nil {
		c.mu.Unlock()
		return nil, errors.New("not running")
	}
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	if err := c.write(mcpMsg{JSONRPC: "2.0", ID: &id, Method: method, Params: params}); err != nil {
		return nil, err
	}
	select {
	case m, ok := <-ch:
		if !ok {
			return nil, errors.New("server exited")
		}
		if m.Error != nil {
			return nil, m.Error
		}
		return m.Result, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *stdioConn) notify(_ context.Context, method string, params any) error {
	return c.write(mcpMsg{JSONRPC: "2.0", Method: method, Params: params})
}

// --- Streamable HTTP ------------------------------------------------------

// httpConn talks to a remote MCP server: each message is a POST, answered
// with JSON or a stream of server-sent events.
type httpConn struct {
	name    string
	url     string
	headers map[string]string
	client  *http.Client

	nextID  atomic.Int64
	mu      sync.Mutex
	session string // Mcp-Session-Id, once initialized
	ready   bool   // initialized: send MCP-Protocol-Version
}

var errSessionExpired = errors.New("session expired")

func (c *httpConn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	res, err := c.roundTrip(ctx, method, params)
	if errors.Is(err, errSessionExpired) && method != "initialize" {
		// The server forgot the session: start a new one, once.
		c.mu.Lock()
		c.session, c.ready = "", false
		c.mu.Unlock()
		if err := mcpInitialize(ctx, c); err != nil {
			return nil, err
		}
		res, err = c.roundTrip(ctx, method, params)
	}
	return res, err
}

func (c *httpConn) roundTrip(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	resp, err := c.post(ctx, mcpMsg{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if method == "initialize" && resp.StatusCode == http.StatusOK {
		c.mu.Lock()
		c.session, c.ready = resp.Header.Get("Mcp-Session-Id"), true
		c.mu.Unlock()
	}
	var m *mcpMsg
	body := io.LimitReader(resp.Body, 4<<20)
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		m, err = readSSE(body, id)
	} else {
		m = &mcpMsg{}
		err = json.NewDecoder(body).Decode(m)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: malformed reply", method)
	}
	if m.Error != nil {
		return nil, m.Error
	}
	return m.Result, nil
}

func (c *httpConn) notify(ctx context.Context, method string, params any) error {
	resp, err := c.post(ctx, mcpMsg{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// post sends one message and returns a successful response.
func (c *httpConn) post(ctx context.Context, m mcpMsg) (*http.Response, error) {
	data, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	c.mu.Lock()
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	if c.ready {
		req.Header.Set("MCP-Protocol-Version", mcpProtocol)
	}
	session := c.session
	c.mu.Unlock()
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound && session != "":
		resp.Body.Close()
		return nil, errSessionExpired
	case resp.StatusCode/100 != 2:
		resp.Body.Close()
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

// readSSE reads server-sent events until the response to request id.
func readSSE(r io.Reader, id int64) (*mcpMsg, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var data strings.Builder
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case line == "" && data.Len() > 0:
			var m mcpMsg
			if json.Unmarshal([]byte(data.String()), &m) == nil && m.ID != nil && *m.ID == id {
				return &m, nil
			}
			data.Reset()
		}
	}
	if data.Len() > 0 {
		var m mcpMsg
		if json.Unmarshal([]byte(data.String()), &m) == nil && m.ID != nil && *m.ID == id {
			return &m, nil
		}
	}
	return nil, errors.New("no response in event stream")
}

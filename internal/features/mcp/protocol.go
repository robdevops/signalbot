package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robdevops/signalbot/internal/kit/httpx"
)

// conn is a connection to one MCP server.
type conn interface {
	call(ctx context.Context, method string, params any) (json.RawMessage, error)
	notify(ctx context.Context, method string, params any) error
}

// RPCError is a JSON-RPC error from an MCP server.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// initialize performs the MCP handshake.
func initialize(ctx context.Context, c conn, version string) error {
	if _, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": Protocol,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "signalbot", "version": version},
	}); err != nil {
		return err
	}
	return c.notify(ctx, "notifications/initialized", nil)
}

const (
	// Protocol is the MCP protocol version this client speaks.
	Protocol = "2025-06-18"
	// CallTimeout bounds one request to a server.
	CallTimeout = 30 * time.Second
)

// Server is how to reach one MCP server: a URL for a remote one, or a
// command for a local one.
type Server struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env"`
}

// Client is a connection to one server.
type Client struct {
	conn  conn
	stdio *stdioConn // nil for a remote server
}

// Dial connects to the server named name and does the MCP handshake,
// sending version as the client's version. A local server is started.
func Dial(ctx context.Context, name string, s Server, version string) (*Client, error) {
	c := &Client{}
	if s.URL != "" {
		c.conn = &httpConn{name: name, url: s.URL, headers: s.Headers, client: httpx.NewClient(), version: version}
	} else {
		c.stdio = &stdioConn{name: name, srv: s, version: version}
		if err := c.stdio.start(ctx); err != nil {
			return nil, err
		}
		c.conn = c.stdio
	}
	ictx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	if err := initialize(ictx, c.conn, version); err != nil {
		return nil, err
	}
	return c, nil
}

// Local reports whether the server runs as a local command.
func (c *Client) Local() bool { return c.stdio != nil }

// Supervise restarts a local server whenever it exits, until ctx ends.
func (c *Client) Supervise(ctx context.Context) {
	if c.stdio != nil {
		c.stdio.supervise(ctx)
	}
}

// Tools lists every tool the server offers by name, following its pages.
func (c *Client) Tools(ctx context.Context) (map[string]mcpTool, error) {
	ctx, cancel := context.WithTimeout(ctx, CallTimeout)
	defer cancel()
	tools := map[string]mcpTool{}
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, err := c.conn.call(ctx, "tools/list", params)
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

// httpConn talks to a remote MCP server: each message is a POST, answered
// with JSON or a stream of server-sent events.
type httpConn struct {
	name    string
	url     string
	headers map[string]string
	client  *http.Client
	version string // clientInfo, sent again when the session is renewed

	nextID  atomic.Int64
	mu      sync.Mutex
	session string // Mcp-Session-Id, once initialized
	ready   bool   // initialized: send MCP-Protocol-Version
}

var errSessionExpired = errors.New("session expired")

func (c *httpConn) roundTrip(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	resp, err := c.post(ctx, message{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if method == "initialize" && resp.StatusCode == http.StatusOK {
		c.mu.Lock()
		c.session, c.ready = resp.Header.Get("Mcp-Session-Id"), true
		c.mu.Unlock()
	}
	var m *message
	body := io.LimitReader(resp.Body, 4<<20)
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		m, err = readSSE(body, id)
	} else {
		m = &message{}
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

// post sends one message and returns a successful response.
func (c *httpConn) post(ctx context.Context, m message) (*http.Response, error) {
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
		req.Header.Set("MCP-Protocol-Version", Protocol)
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
func readSSE(r io.Reader, id int64) (*message, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	var data strings.Builder
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "data:"):
			if data.Len() > 0 {
				data.WriteByte('\n') // lines of one event join with newlines
			}
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case line == "" && data.Len() > 0:
			var m message
			if json.Unmarshal([]byte(data.String()), &m) == nil && m.ID != nil && *m.ID == id {
				return &m, nil
			}
			data.Reset()
		}
	}
	if data.Len() > 0 {
		var m message
		if json.Unmarshal([]byte(data.String()), &m) == nil && m.ID != nil && *m.ID == id {
			return &m, nil
		}
	}
	return nil, errors.New("no response in event stream")
}

func (c *httpConn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	res, err := c.roundTrip(ctx, method, params)
	if errors.Is(err, errSessionExpired) && method != "initialize" {
		// The server forgot the session: start a new one, once.
		c.mu.Lock()
		c.session, c.ready = "", false
		c.mu.Unlock()
		if err := initialize(ctx, c, c.version); err != nil {
			return nil, err
		}
		res, err = c.roundTrip(ctx, method, params)
	}
	return res, err
}

func (c *httpConn) notify(ctx context.Context, method string, params any) error {
	resp, err := c.post(ctx, message{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

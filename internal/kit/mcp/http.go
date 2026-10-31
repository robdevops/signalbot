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
)

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

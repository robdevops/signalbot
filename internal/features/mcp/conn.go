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
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/robdevops/signalbot/internal/kit/logx"
)

// conn is a connection to one MCP server.
type conn interface {
	call(ctx context.Context, method string, params any) (json.RawMessage, error)
	notify(ctx context.Context, method string, params any) error
}

// rpcError is a JSON-RPC error from an MCP server.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  any             `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// --- stdio --------------------------------------------------------------

// stdioConn runs a local MCP server and talks to it over stdin and stdout,
// one JSON message per line.
type stdioConn struct {
	name, version string
	cfg           server
	redact        func(string) string // scrubs names and IDs from its output before it is logged

	nextID  atomic.Int64
	mu      sync.Mutex // guards stdin, pending and done
	stdin   io.WriteCloser
	pending map[int64]chan rpcMsg
	done    chan struct{} // closed when the current process exits
}

func (c *stdioConn) start(ctx context.Context) error {
	cmd := exec.CommandContext(ctx, c.cfg.Command, c.cfg.Args...)
	// Its whole environment: PATH, HOME and its configured env, never the
	// bot's own, which holds the API key.
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	for k, v := range c.cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
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
	c.stdin, c.pending, c.done = stdin, map[int64]chan rpcMsg{}, done
	c.mu.Unlock()

	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			logx.Debugf("MCP server %s: %s", c.name, c.redact(sc.Text()))
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
		logx.Warnf("MCP server %s exited; restarting in %s", c.name, backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 32*time.Second)
		if err := c.start(ctx); err != nil {
			logx.Warnf("MCP server %s failed to start: %v", c.name, err)
			continue // c.done is still the exited process's, so it retries
		}
		ictx, cancel := context.WithTimeout(ctx, callTimeout)
		if _, err := initialize(ictx, c, c.version); err != nil {
			logx.Warnf("MCP server %s failed to initialize: %v", c.name, err)
		} else {
			backoff = time.Second
		}
		cancel()
	}
}

func (c *stdioConn) dispatch(line []byte) {
	var m rpcMsg
	if json.Unmarshal(line, &m) != nil {
		logx.Warnf("bad JSON from MCP server %s (%d bytes), ignored", c.name, len(line))
		return
	}
	switch {
	case m.ID != nil && m.Method != "":
		// A request from the server: answer pings, refuse the rest.
		reply := rpcMsg{JSONRPC: "2.0", ID: m.ID, Result: json.RawMessage(`{}`)}
		if m.Method != "ping" {
			reply = rpcMsg{JSONRPC: "2.0", ID: m.ID, Error: &rpcError{Code: -32601, Message: "method not supported"}}
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

func (c *stdioConn) write(m rpcMsg) error {
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
	ch := make(chan rpcMsg, 1)
	c.mu.Lock()
	if c.stdin == nil {
		c.mu.Unlock()
		return nil, errors.New("not running")
	}
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	if err := c.write(rpcMsg{JSONRPC: "2.0", ID: &id, Method: method, Params: params}); err != nil {
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
	return c.write(rpcMsg{JSONRPC: "2.0", Method: method, Params: params})
}

// --- Streamable HTTP ------------------------------------------------------

// httpConn talks to a remote MCP server: each message is a POST, answered
// with JSON or a stream of server-sent events.
type httpConn struct {
	url, version string
	headers      map[string]string
	client       *http.Client

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
		if _, err := initialize(ctx, c, c.version); err != nil {
			return nil, err
		}
		res, err = c.roundTrip(ctx, method, params)
	}
	return res, err
}

func (c *httpConn) roundTrip(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	resp, err := c.post(ctx, rpcMsg{JSONRPC: "2.0", ID: &id, Method: method, Params: params})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if method == "initialize" && resp.StatusCode == http.StatusOK {
		c.mu.Lock()
		c.session, c.ready = resp.Header.Get("Mcp-Session-Id"), true
		c.mu.Unlock()
	}
	var m *rpcMsg
	body := io.LimitReader(resp.Body, 4<<20)
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		m, err = readSSE(body, id)
	} else {
		m = &rpcMsg{}
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
	resp, err := c.post(ctx, rpcMsg{JSONRPC: "2.0", Method: method, Params: params})
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// post sends one message and returns a successful response.
func (c *httpConn) post(ctx context.Context, m rpcMsg) (*http.Response, error) {
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
		req.Header.Set("MCP-Protocol-Version", protocol)
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
func readSSE(r io.Reader, id int64) (*rpcMsg, error) {
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
			var m rpcMsg
			if json.Unmarshal([]byte(data.String()), &m) == nil && m.ID != nil && *m.ID == id {
				return &m, nil
			}
			data.Reset()
		}
	}
	if data.Len() > 0 {
		var m rpcMsg
		if json.Unmarshal([]byte(data.String()), &m) == nil && m.ID != nil && *m.ID == id {
			return &m, nil
		}
	}
	return nil, errors.New("no response in event stream")
}

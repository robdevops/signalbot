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

// A local server's call fails with errNotRunning while it is down, and
// errExited if it exits before answering.
var (
	errNotRunning = errors.New("not running")
	errExited     = errors.New("server exited")
)

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
	name    string
	bot     botInfo // what initialize tells the server
	cfg     server
	redact  func(string) string              // scrubs names and IDs from its output before it is logged
	prefix  string                           // an addon's: it gets the bot's variables that start with this, and its stderr is only counted
	updated chan struct{}                    // an addon's: marked, without waiting, when it says a resource was updated
	ready   func(context.Context, handshake) // an addon's: called each time it has started again and initialized

	nextID  atomic.Int64
	mu      sync.Mutex // guards stdin, pending, done and the stderr counts
	stdin   io.WriteCloser
	pending map[int64]chan rpcMsg
	done    chan struct{} // closed when the current process exits

	errLines, errBytes int       // an addon's stderr, since it was last logged
	errLogged          time.Time // when it was
}

const (
	// maxLine caps a line from a server; a longer one gets it restarted.
	maxLine = 4 << 20
	// maxStderrLine caps what is logged of a line on a server's stderr.
	maxStderrLine = 4 << 10
	// stopWait is how long a server has to exit after its stdin closes,
	// and again after SIGTERM, before SIGKILL.
	stopWait = 2 * time.Second
)

// start starts the server, in a process group of its own, stopped when ctx
// ends: its stdin is closed, then the group gets SIGTERM, then SIGKILL. The
// group is killed when the server exits, so what it started stops too.
func (c *stdioConn) start(ctx context.Context) error {
	cmd := exec.Command(c.cfg.Command, c.cfg.Args...)
	// Its whole environment: PATH, HOME and its configured env, or an
	// addon's prefixed variables, never the bot's own, which hold the API
	// key.
	if c.prefix != "" {
		cmd.Env = addonEnv(c.prefix)
	} else {
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
		for k, v := range c.cfg.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}
	setGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	// Its own pipes, not exec's, so Wait never closes them under a read:
	// they end when every process writing to them has.
	outR, outW, err := os.Pipe()
	if err != nil {
		return err
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		outR.Close()
		outW.Close()
		return err
	}
	cmd.Stdout, cmd.Stderr = outW, errW
	err = cmd.Start()
	outW.Close()
	errW.Close()
	if err != nil {
		outR.Close()
		errR.Close()
		return err
	}
	done, exited := make(chan struct{}), make(chan struct{})
	c.mu.Lock()
	c.stdin, c.pending, c.done = stdin, map[int64]chan rpcMsg{}, done
	c.mu.Unlock()

	go c.readStderr(errR)
	go func() {
		cmd.Wait()
		signalGroup(cmd.Process, syscall.SIGKILL) // anything it left running
		close(exited)
	}()
	go func() {
		select {
		case <-exited:
			return
		case <-ctx.Done():
		}
		stdin.Close()
		for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
			select {
			case <-exited:
				return
			case <-time.After(stopWait):
				signalGroup(cmd.Process, sig)
			}
		}
	}()
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(make([]byte, 64<<10), maxLine)
		for sc.Scan() {
			c.dispatch(sc.Bytes())
		}
		if errors.Is(sc.Err(), bufio.ErrTooLong) {
			logx.Warnf("MCP server %s wrote a line over %d MiB, so it is stopped and restarted", c.name, maxLine>>20)
			signalGroup(cmd.Process, syscall.SIGKILL)
		}
		outR.Close()
		<-exited
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

// readStderr reads the server's stderr in pieces, so no line is too long
// for it. A plain server's lines are logged at debug, scrubbed and cut; an
// addon's are only counted, since it may write what it was sent, and what
// was counted since the last count logged is logged once it exits.
func (c *stdioConn) readStderr(r io.ReadCloser) {
	defer r.Close()
	br := bufio.NewReaderSize(r, maxStderrLine)
	var line []byte
	for {
		piece, more, err := br.ReadLine()
		if err != nil {
			if c.prefix != "" {
				c.flushStderr()
			}
			return
		}
		if c.prefix != "" {
			c.countStderr(len(piece), !more)
			continue
		}
		line = append(line, piece[:min(len(piece), maxStderrLine-len(line))]...)
		if !more {
			logx.Debugf("MCP server %s: %s", c.name, c.redact(string(line)))
			line = line[:0]
		}
	}
}

// countStderr counts n bytes of an addon's stderr, and a line if end, and
// logs the counts at most once a minute.
func (c *stdioConn) countStderr(n int, end bool) {
	c.mu.Lock()
	c.errBytes += n
	if end {
		c.errLines++
	}
	lines, bytes := c.errLines, c.errBytes
	due := lines > 0 && time.Since(c.errLogged) >= time.Minute
	if due {
		c.errLines, c.errBytes, c.errLogged = 0, 0, time.Now()
	}
	c.mu.Unlock()
	if due {
		c.logStderr(lines, bytes)
	}
}

// flushStderr logs what was counted of an addon's stderr since the count
// was last logged, if anything.
func (c *stdioConn) flushStderr() {
	c.mu.Lock()
	lines, bytes := c.errLines, c.errBytes
	pending := lines > 0 || bytes > 0
	if pending {
		c.errLines, c.errBytes, c.errLogged = 0, 0, time.Now()
	}
	c.mu.Unlock()
	if pending {
		c.logStderr(max(lines, 1), bytes) // a last line without a line break is still a line
	}
}

// logStderr logs a count of an addon's stderr.
func (c *stdioConn) logStderr(lines, bytes int) {
	unit := "lines"
	if lines == 1 {
		unit = "line"
	}
	logx.Infof("addon %s wrote %d %s (%d bytes) to stderr", c.name, lines, unit, bytes)
}

// restartDelay is the first wait before restarting a local server that
// exited, doubling to 32s while it keeps failing; tests shorten it.
var restartDelay = time.Second

// supervise restarts the server whenever it exits, until ctx ends, and
// calls ready, if set, each time one has initialized.
func (c *stdioConn) supervise(ctx context.Context) {
	backoff := restartDelay
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
			logx.Warnf("MCP server %s failed to start: %s", c.name, describe(err, c.prefix != ""))
			continue // c.done is still the exited process's, so it retries
		}
		ictx, cancel := context.WithTimeout(ctx, callTimeout)
		hs, err := initialize(ictx, c, c.bot)
		cancel()
		if err != nil {
			logx.Warnf("MCP server %s failed to initialize: %s", c.name, describe(err, c.prefix != ""))
			continue
		}
		backoff = restartDelay
		if c.ready != nil {
			c.ready(ctx, hs)
		}
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
	case m.Method == "notifications/resources/updated" && c.updated != nil:
		// Any resource: the spec lets it be one under the one subscribed.
		select {
		case c.updated <- struct{}{}:
		default:
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
		return errNotRunning
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
		return nil, errNotRunning
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
			return nil, errExited
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
	url     string
	bot     botInfo // what initialize tells the server
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
		if _, err := initialize(ctx, c, c.bot); err != nil {
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

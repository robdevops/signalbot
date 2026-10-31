package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/robdevops/signalbot/internal/kit/logx"
)

// stdioConn runs a local MCP server and talks to it over stdin and stdout,
// one JSON message per line.
type stdioConn struct {
	name    string
	srv     Server
	version string // clientInfo, sent again after a restart

	nextID  atomic.Int64
	mu      sync.Mutex // guards stdin, pending and done
	stdin   io.WriteCloser
	pending map[int64]chan message
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
	cmd := exec.CommandContext(ctx, c.srv.Command, c.srv.Args...)
	cmd.Env = stdioEnv(c.srv.Env)
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
	c.stdin, c.pending, c.done = stdin, map[int64]chan message{}, done
	c.mu.Unlock()

	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			logx.Debugf("MCP server %s: %s", c.name, sc.Text())
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
			c.mu.Lock()
			c.done = closedChan()
			c.mu.Unlock()
			continue
		}
		ictx, cancel := context.WithTimeout(ctx, CallTimeout)
		if err := initialize(ictx, c, c.version); err != nil {
			logx.Warnf("MCP server %s failed to initialize: %v", c.name, err)
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
	var m message
	if json.Unmarshal(line, &m) != nil {
		logx.Warnf("bad JSON from MCP server %s (%d bytes), ignored", c.name, len(line))
		return
	}
	switch {
	case m.ID != nil && m.Method != "":
		// A request from the server: answer pings, refuse the rest.
		reply := message{JSONRPC: "2.0", ID: m.ID, Result: json.RawMessage(`{}`)}
		if m.Method != "ping" {
			reply = message{JSONRPC: "2.0", ID: m.ID, Error: &RPCError{Code: -32601, Message: "method not supported"}}
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

func (c *stdioConn) write(m message) error {
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
	ch := make(chan message, 1)
	c.mu.Lock()
	if c.stdin == nil {
		c.mu.Unlock()
		return nil, errors.New("not running")
	}
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	if err := c.write(message{JSONRPC: "2.0", ID: &id, Method: method, Params: params}); err != nil {
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
	return c.write(message{JSONRPC: "2.0", Method: method, Params: params})
}

// Package signal is a JSON-RPC client for the signal-cli daemon socket.
package signal

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robdevops/signalbot/internal/kit/logx"
)

type rpcMsg struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Envelope *Envelope `json:"envelope"`
}

// maxLine caps one JSON-RPC frame from signal-cli. Message bodies, mention
// lists and quoted text are all sender-controlled parts of a frame.
const maxLine = 1 << 20

// Client is a JSON-RPC client for a signal-cli daemon socket.
type Client struct {
	addr    string
	nextID  atomic.Int64
	mu      sync.Mutex // guards conn writes and pending
	conn    net.Conn
	pending map[string]chan *rpcMsg
	selfID  atomic.Value // string: the account's ACI, else its number

	// OnConnect, if set before Run, is called after each (re)connection once
	// the bot's account id is known. It must not block.
	OnConnect func()
}

func New(addr string) *Client {
	return &Client{addr: addr, pending: map[string]chan *rpcMsg{}}
}

// SelfID is the bot's account id (ACI, else number), or "" until listAccounts answers.
func (s *Client) SelfID() string {
	id, _ := s.selfID.Load().(string)
	return id
}

// WaitSelf returns the bot's account id, waiting briefly for startup to learn it.
func (s *Client) WaitSelf(max time.Duration) string {
	deadline := time.Now().Add(max)
	for {
		if id := s.SelfID(); id != "" || time.Now().After(deadline) {
			return id
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// network picks unix for anything that looks like a path ("/run/x",
// "run/x", "./x", or an abstract "@x") and tcp for host:port.
func (s *Client) network() string {
	if _, _, err := net.SplitHostPort(s.addr); err == nil && !strings.ContainsRune(s.addr, '/') && !strings.HasPrefix(s.addr, "@") {
		return "tcp"
	}
	return "unix"
}

func (s *Client) dial() (net.Conn, error) {
	return net.DialTimeout(s.network(), s.addr, 10*time.Second)
}

// Run connects, learns the bot's own account id, and calls handle for every
// received envelope. It reconnects with backoff until ctx is cancelled.
func (s *Client) Run(ctx context.Context, handle func(*Envelope)) {
	backoff := time.Second
	for ctx.Err() == nil {
		conn, err := s.dial()
		if err != nil {
			logx.Warnf("signal-cli connect failed: %v; retrying in %s", err, backoff)
		} else {
			began := time.Now()
			s.serve(ctx, conn, handle)
			// Only a connection that stayed up resets the backoff; one that
			// dies at once (say listAccounts keeps failing) must not spin.
			if time.Since(began) > time.Minute {
				backoff = time.Second
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (s *Client) serve(ctx context.Context, conn net.Conn, handle func(*Envelope)) {
	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()
	logx.Infof("connected to signal-cli at %s", s.addr)

	defer context.AfterFunc(ctx, func() { conn.Close() })()
	go func() {
		rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		id, err := s.listAccounts(rctx)
		if err != nil {
			logx.Errorf("listAccounts failed: %v", err)
			conn.Close()
			return
		}
		s.selfID.Store(id)
		logx.Infof("bot account %s", id)
		if s.OnConnect != nil {
			s.OnConnect()
		}
	}()

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), maxLine)
	for sc.Scan() {
		s.dispatch(sc.Bytes(), handle)
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		logx.Warnf("signal-cli read failed: %v; reconnecting", err) // a frame over maxLine is bufio.ErrTooLong
	}
	conn.Close()
	s.mu.Lock()
	s.conn = nil
	for id, ch := range s.pending {
		close(ch)
		delete(s.pending, id)
	}
	s.mu.Unlock()
}

func (s *Client) dispatch(line []byte, handle func(*Envelope)) {
	var m rpcMsg
	if err := json.Unmarshal(line, &m); err != nil {
		logx.Warnf("bad JSON from signal-cli (%d bytes), ignored", len(line)) // not err: it can quote the frame
		return
	}
	switch {
	case m.Method == "" && len(m.ID) > 0 && string(m.ID) != "null":
		s.mu.Lock()
		ch := s.pending[string(m.ID)]
		delete(s.pending, string(m.ID))
		s.mu.Unlock()
		if ch != nil {
			ch <- &m
		} else {
			s.handleUnmatched(&m)
		}
	case m.Method == "receive":
		var p rpcMsg
		if json.Unmarshal(m.Params, &p) == nil && p.Envelope != nil {
			handle(p.Envelope)
		}
	}
}

// write sends one JSON-RPC request.
func (s *Client) write(id int64, method string, params any) error {
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return errors.New("not connected")
	}
	_, err = s.conn.Write(append(b, '\n'))
	return err
}

// call sends a request and waits for its response.
func (s *Client) call(ctx context.Context, method string, params any) (*rpcMsg, error) {
	id := s.nextID.Add(1)
	key := fmt.Sprint(id)
	ch := make(chan *rpcMsg, 1)
	s.mu.Lock()
	s.pending[key] = ch
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.pending, key); s.mu.Unlock() }()
	if err := s.write(id, method, params); err != nil {
		return nil, err
	}
	select {
	case resp, ok := <-ch:
		if !ok {
			return nil, errors.New("connection closed")
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("rpc error %d: %s", resp.Error.Code, resp.Error.Message)
		}
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// listAccounts returns the account's ACI, else its number.
func (s *Client) listAccounts(ctx context.Context) (string, error) {
	resp, err := s.call(ctx, "listAccounts", nil)
	if err != nil {
		return "", err
	}
	var accts []struct {
		Number string `json:"number"`
		ACI    string `json:"aci"`
	}
	if err := json.Unmarshal(resp.Result, &accts); err != nil {
		return "", err
	}
	if len(accts) == 0 || cmp.Or(accts[0].ACI, accts[0].Number) == "" {
		return "", errors.New("no account registered")
	}
	return cmp.Or(accts[0].ACI, accts[0].Number), nil
}

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Mention struct {
	Name   string `json:"name"`
	Number string `json:"number"`
	UUID   string `json:"uuid"`
	Start  int    `json:"start"`
	Length int    `json:"length"`
}

type Quote struct {
	ID         int64  `json:"id"`
	Author     string `json:"author"`
	AuthorUUID string `json:"authorUuid"`
	Text       string `json:"text"`
}

type GroupInfo struct {
	GroupID string `json:"groupId"`
}

type DataMessage struct {
	Message   string     `json:"message"`
	Mentions  []Mention  `json:"mentions"`
	Quote     *Quote     `json:"quote"`
	GroupInfo *GroupInfo `json:"groupInfo"`
}

type Envelope struct {
	Source       string       `json:"source"`
	SourceNumber string       `json:"sourceNumber"`
	SourceUUID   string       `json:"sourceUuid"`
	SourceName   string       `json:"sourceName"`
	DataMessage  *DataMessage `json:"dataMessage"`
}

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

// Signal is a JSON-RPC client for a signal-cli daemon socket.
type Signal struct {
	addr    string
	nextID  atomic.Int64
	mu      sync.Mutex // guards conn writes and pending
	conn    net.Conn
	pending map[string]chan *rpcMsg
	selfID  atomic.Value // string
}

func NewSignal(addr string) *Signal {
	return &Signal{addr: addr, pending: map[string]chan *rpcMsg{}}
}

func (s *Signal) SelfID() string {
	v, _ := s.selfID.Load().(string)
	return v
}

// WaitSelf returns the bot's account id, waiting briefly for startup to learn it.
func (s *Signal) WaitSelf(max time.Duration) string {
	deadline := time.Now().Add(max)
	for {
		if id := s.SelfID(); id != "" || time.Now().After(deadline) {
			return id
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *Signal) dial() (net.Conn, error) {
	network := "tcp"
	if strings.HasPrefix(s.addr, "/") || strings.HasPrefix(s.addr, "@") || strings.HasPrefix(s.addr, ".") {
		network = "unix"
	}
	return net.DialTimeout(network, s.addr, 10*time.Second)
}

// Run connects, learns the bot's own account id, and calls handle for every
// received envelope. It reconnects with backoff until ctx is cancelled.
func (s *Signal) Run(ctx context.Context, handle func(*Envelope)) {
	backoff := time.Second
	for ctx.Err() == nil {
		conn, err := s.dial()
		if err != nil {
			slog.Error("signal connect failed", "err", err, "retry_in", backoff)
		} else {
			backoff = time.Second
			s.serve(ctx, conn, handle)
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

func (s *Signal) serve(ctx context.Context, conn net.Conn, handle func(*Envelope)) {
	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()
	slog.Info("connected to signal-cli", "addr", s.addr)

	go func() { <-ctx.Done(); conn.Close() }()
	go func() {
		rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		id, err := s.listAccounts(rctx)
		if err != nil {
			slog.Error("listAccounts failed", "err", err)
			conn.Close()
			return
		}
		s.selfID.Store(id)
		slog.Info("bot account", "id", id)
	}()

	r := bufio.NewReaderSize(conn, 64*1024)
	for {
		line, err := r.ReadBytes('\n')
		if len(line) > 0 {
			s.dispatch(line, handle)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && ctx.Err() == nil {
				slog.Error("signal read failed", "err", err)
			}
			break
		}
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

func (s *Signal) dispatch(line []byte, handle func(*Envelope)) {
	var m rpcMsg
	if err := json.Unmarshal(line, &m); err != nil {
		slog.Warn("bad json from signal-cli", "err", err)
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
			logUnmatchedResponse(&m)
		}
	case m.Method == "receive":
		var p rpcMsg
		if json.Unmarshal(m.Params, &p) == nil && p.Envelope != nil {
			handle(p.Envelope)
		}
	case m.Envelope != nil:
		handle(m.Envelope)
	}
}

func (s *Signal) write(v any) error {
	b, err := json.Marshal(v)
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
func (s *Signal) call(ctx context.Context, method string, params any) (*rpcMsg, error) {
	id := s.nextID.Add(1)
	key := fmt.Sprint(id)
	ch := make(chan *rpcMsg, 1)
	s.mu.Lock()
	s.pending[key] = ch
	s.mu.Unlock()
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	if err := s.write(req); err != nil {
		s.mu.Lock()
		delete(s.pending, key)
		s.mu.Unlock()
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
		s.mu.Lock()
		delete(s.pending, key)
		s.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (s *Signal) listAccounts(ctx context.Context) (string, error) {
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
	if len(accts) == 0 {
		return "", errors.New("no accounts registered")
	}
	if accts[0].ACI != "" {
		return accts[0].ACI, nil
	}
	return accts[0].Number, nil
}

// Target addresses a chat: a group or a single recipient.
type Target struct {
	GroupID string
	User    string
}

func (t Target) params(extra map[string]any) map[string]any {
	p := map[string]any{}
	if t.GroupID != "" {
		p["groupId"] = t.GroupID
	} else {
		p["recipient"] = []string{t.User}
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

func (s *Signal) Send(t Target, msg string) error {
	return s.write(map[string]any{"jsonrpc": "2.0", "id": s.nextID.Add(1), "method": "send",
		"params": t.params(map[string]any{"message": msg})})
}

func (s *Signal) Typing(t Target) {
	_ = s.write(map[string]any{"jsonrpc": "2.0", "id": s.nextID.Add(1), "method": "sendTyping", "params": t.params(nil)})
}

// logUnmatchedResponse reports the outcome of fire-and-forget requests (send, sendTyping).
func logUnmatchedResponse(m *rpcMsg) {
	if m.Error != nil {
		slog.Error("signal-cli request failed", "id", string(m.ID), "code", m.Error.Code, "err", m.Error.Message)
		return
	}
	var r struct {
		Results []struct {
			Type string `json:"type"`
		} `json:"results"`
	}
	if json.Unmarshal(m.Result, &r) != nil {
		return
	}
	for _, res := range r.Results {
		if res.Type != "" && res.Type != "SUCCESS" {
			slog.Warn("signal-cli send not delivered", "id", string(m.ID), "result", res.Type)
		}
	}
}

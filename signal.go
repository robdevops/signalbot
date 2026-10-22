package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// maxLine caps one JSON-RPC frame from signal-cli. Message bodies, mention
// lists and quoted text are all sender-controlled parts of a frame.
const maxLine = 1 << 20

// Signal is a JSON-RPC client for a signal-cli daemon socket.
type Signal struct {
	addr    string
	nextID  atomic.Int64
	mu      sync.Mutex // guards conn writes and pending
	conn    net.Conn
	pending map[string]chan *rpcMsg
	selfID  atomic.Value // string: the account's ACI, else its number
	selfIDs atomic.Value // []string: every id the account is known by
}

func NewSignal(addr string) *Signal {
	return &Signal{addr: addr, pending: map[string]chan *rpcMsg{}}
}

func (s *Signal) SelfID() string {
	v, _ := s.selfID.Load().(string)
	return v
}

// SelfIDs is every id the bot's account is known by (ACI and number).
func (s *Signal) SelfIDs() []string {
	if v, _ := s.selfIDs.Load().([]string); len(v) > 0 {
		return v
	}
	if id := s.SelfID(); id != "" {
		return []string{id}
	}
	return nil
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

// network picks unix for anything that looks like a path ("/run/x",
// "run/x", "./x", or an abstract "@x") and tcp for host:port.
func (s *Signal) network() string {
	if strings.ContainsRune(s.addr, '/') || strings.HasPrefix(s.addr, "@") {
		return "unix"
	}
	if _, _, err := net.SplitHostPort(s.addr); err == nil {
		return "tcp"
	}
	return "unix"
}

func (s *Signal) dial() (net.Conn, error) {
	return net.DialTimeout(s.network(), s.addr, 10*time.Second)
}

// Run connects, learns the bot's own account id, and calls handle for every
// received envelope. It reconnects with backoff until ctx is cancelled.
func (s *Signal) Run(ctx context.Context, handle func(*Envelope)) {
	backoff := time.Second
	for ctx.Err() == nil {
		conn, err := s.dial()
		if err != nil {
			logf("signal-cli connect failed: %v; retrying in %s", err, backoff)
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

func (s *Signal) serve(ctx context.Context, conn net.Conn, handle func(*Envelope)) {
	s.mu.Lock()
	s.conn = conn
	s.mu.Unlock()
	logf("connected to signal-cli at %s", s.addr)

	go func() { <-ctx.Done(); conn.Close() }()
	go func() {
		rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		ids, err := s.listAccounts(rctx)
		if err != nil {
			logf("listAccounts failed: %v", err)
			conn.Close()
			return
		}
		s.selfIDs.Store(ids)
		s.selfID.Store(ids[0])
		logf("bot account %s", ids[0])
	}()

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), maxLine)
	for sc.Scan() {
		s.dispatch(sc.Bytes(), handle)
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		if errors.Is(err, bufio.ErrTooLong) {
			logf("signal-cli sent a frame over %d bytes; reconnecting", maxLine)
		} else if !errors.Is(err, io.EOF) {
			logf("signal-cli read failed: %v", err)
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
		logf("bad JSON from signal-cli: %v", err)
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

// listAccounts returns the account's ids, primary (ACI, else number) first.
func (s *Signal) listAccounts(ctx context.Context) ([]string, error) {
	resp, err := s.call(ctx, "listAccounts", nil)
	if err != nil {
		return nil, err
	}
	var accts []struct {
		Number string `json:"number"`
		ACI    string `json:"aci"`
	}
	if err := json.Unmarshal(resp.Result, &accts); err != nil {
		return nil, err
	}
	if len(accts) == 0 {
		return nil, errors.New("no accounts registered")
	}
	var ids []string
	for _, id := range []string{accts[0].ACI, accts[0].Number} {
		if id != "" {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, errors.New("account has neither an ACI nor a number")
	}
	return ids, nil
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

// Send posts msg; mentions are signal-cli specs ("start:length:uuid") for @names in msg.
func (s *Signal) Send(t Target, msg string, mentions []string) error {
	extra := map[string]any{"message": msg}
	if len(mentions) > 0 {
		extra["mention"] = mentions
	}
	return s.write(map[string]any{"jsonrpc": "2.0", "id": s.nextID.Add(1), "method": "send",
		"params": t.params(extra)})
}

func (s *Signal) Typing(t Target) {
	_ = s.write(map[string]any{"jsonrpc": "2.0", "id": s.nextID.Add(1), "method": "sendTyping", "params": t.params(nil)})
}

// handleUnmatched reports the outcome of fire-and-forget requests (send,
// sendTyping).
func (s *Signal) handleUnmatched(m *rpcMsg) {
	if m.Error != nil {
		logf("signal-cli request %s failed: %s", m.ID, m.Error.Message)
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
			logf("signal-cli request %s not delivered: %s", m.ID, res.Type)
		}
	}
}

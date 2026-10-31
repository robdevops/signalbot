package main

import (
	"bufio"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
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
	Author      string       `json:"author"`
	AuthorUUID  string       `json:"authorUuid"`
	Text        string       `json:"text"`
	Attachments []Attachment `json:"attachments"`
}

// Attachment is a file on a message. A quote's attachments carry only a
// small thumbnail of the original.
type Attachment struct {
	ContentType string      `json:"contentType"`
	ID          string      `json:"id"`
	Size        int         `json:"size"`
	Thumbnail   *Attachment `json:"thumbnail"`
}

// Sticker names a sticker by its pack and its index in the pack.
type Sticker struct {
	PackID    string `json:"packId"`
	StickerID int    `json:"stickerId"`
}

// image is the attachment itself if it is an image the model can be shown,
// else its thumbnail if that is, else nil.
func (a Attachment) image() *Attachment {
	for _, c := range []*Attachment{&a, a.Thumbnail} {
		if c == nil || c.ID == "" || c.Size > maxImageBytes {
			continue
		}
		switch c.ContentType {
		case "image/png", "image/jpeg", "image/gif", "image/webp":
			return c
		}
	}
	return nil
}

type GroupInfo struct {
	GroupID string `json:"groupId"`
	Type    string `json:"type"` // "UPDATE" when the group's members, name or description changed
}

type DataMessage struct {
	Message     string       `json:"message"`
	Mentions    []Mention    `json:"mentions"`
	Quote       *Quote       `json:"quote"`
	GroupInfo   *GroupInfo   `json:"groupInfo"`
	Attachments []Attachment `json:"attachments"`
	Sticker     *Sticker     `json:"sticker"`

	// ExpiresInSeconds is the chat's disappearing-messages timer, 0 if off.
	ExpiresInSeconds int `json:"expiresInSeconds"`
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

	// onConnect, if set before Run, is called after each (re)connection once
	// the bot's account id is known. It must not block.
	onConnect func()
}

func NewSignal(addr string) *Signal {
	return &Signal{addr: addr, pending: map[string]chan *rpcMsg{}}
}

// SelfID is the bot's account id (ACI, else number), or "" until listAccounts answers.
func (s *Signal) SelfID() string {
	id, _ := s.selfID.Load().(string)
	return id
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
	if _, _, err := net.SplitHostPort(s.addr); err == nil && !strings.ContainsRune(s.addr, '/') && !strings.HasPrefix(s.addr, "@") {
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
			warnf("signal-cli connect failed: %v; retrying in %s", err, backoff)
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
	infof("connected to signal-cli at %s", s.addr)

	defer context.AfterFunc(ctx, func() { conn.Close() })()
	go func() {
		rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		id, err := s.listAccounts(rctx)
		if err != nil {
			errorf("listAccounts failed: %v", err)
			conn.Close()
			return
		}
		s.selfID.Store(id)
		infof("bot account %s", id)
		if s.onConnect != nil {
			s.onConnect()
		}
	}()

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), maxLine)
	for sc.Scan() {
		s.dispatch(sc.Bytes(), handle)
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		warnf("signal-cli read failed: %v; reconnecting", err) // a frame over maxLine is bufio.ErrTooLong
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
		warnf("bad JSON from signal-cli (%d bytes), ignored", len(line)) // not err: it can quote the frame
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
func (s *Signal) write(id int64, method string, params any) error {
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
func (s *Signal) call(ctx context.Context, method string, params any) (*rpcMsg, error) {
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
	if len(accts) == 0 || cmp.Or(accts[0].ACI, accts[0].Number) == "" {
		return "", errors.New("no account registered")
	}
	return cmp.Or(accts[0].ACI, accts[0].Number), nil
}

// Target addresses a chat: a group or a single recipient.
type Target struct {
	GroupID string
	User    string
}

func (t Target) params() map[string]any {
	if t.GroupID != "" {
		return map[string]any{"groupId": t.GroupID}
	}
	return map[string]any{"recipient": []string{t.User}}
}

// Send posts msg; mentions are signal-cli specs ("start:length:uuid") for @names in msg.
// GetAttachment fetches an attachment's bytes from signal-cli by id, naming
// the chat it arrived in: the group, or for a DM the person who sent it. The
// bot never reads signal-cli's data directory itself.
func (s *Signal) GetAttachment(ctx context.Context, t Target, id string) ([]byte, error) {
	p := map[string]any{"id": id}
	if t.GroupID != "" {
		p["groupId"] = t.GroupID
	} else {
		p["recipient"] = t.User
	}
	resp, err := s.call(ctx, "getAttachment", p)
	if err != nil {
		return nil, err
	}
	var r struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		return nil, errors.New("malformed getAttachment result")
	}
	return base64.StdEncoding.DecodeString(r.Data)
}

// GetSticker fetches a sticker's image (WebP, or APNG if animated) from
// signal-cli, which downloads the packs of stickers it receives.
func (s *Signal) GetSticker(ctx context.Context, st Sticker) ([]byte, error) {
	resp, err := s.call(ctx, "getSticker", map[string]any{"packId": st.PackID, "stickerId": st.StickerID})
	if err != nil {
		return nil, err
	}
	var r struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(resp.Result, &r); err != nil {
		return nil, errors.New("malformed getSticker result")
	}
	return base64.StdEncoding.DecodeString(r.Data)
}

// StickerLabel describes a sticker from its pack's details: its emoji and
// the pack's title, e.g. `: 🕵️ from "My Daily Life"`, or "" if signal-cli
// doesn't know the pack. Both come from whoever made the pack, so they are
// trimmed and the title quoted.
func (s *Signal) StickerLabel(ctx context.Context, st Sticker) string {
	resp, err := s.call(ctx, "listStickerPacks", nil)
	if err != nil {
		return ""
	}
	var packs []struct {
		PackID   string `json:"packId"`
		Title    string `json:"title"`
		Stickers []struct {
			ID    int    `json:"id"`
			Emoji string `json:"emoji"`
		} `json:"stickers"`
	}
	if json.Unmarshal(resp.Result, &packs) != nil {
		return ""
	}
	for _, p := range packs {
		if p.PackID != st.PackID {
			continue
		}
		label := ""
		for _, sk := range p.Stickers {
			if sk.ID == st.StickerID {
				if e := clamp(safeName(sk.Emoji), 8); e != "" {
					label = ": " + e
				}
			}
		}
		if t := safeName(p.Title); t != "" {
			label += " from " + strconv.Quote(t)
		}
		return label
	}
	return ""
}

// Send sends msg, with mentions ("start:length:uuid") and attachments (data:
// URIs, see imageDataURI).
func (s *Signal) Send(t Target, msg string, mentions, attachments []string) error {
	p := t.params()
	p["message"] = msg
	if len(mentions) > 0 {
		p["mention"] = mentions
	}
	if len(attachments) > 0 {
		p["attachments"] = attachments
	}
	return s.write(s.nextID.Add(1), "send", p)
}

func (s *Signal) Typing(t Target) {
	_ = s.write(s.nextID.Add(1), "sendTyping", t.params())
}

// handleUnmatched reports the outcome of fire-and-forget requests (send,
// sendTyping).
func (s *Signal) handleUnmatched(m *rpcMsg) {
	if m.Error != nil {
		errorf("signal-cli request %s failed: %s", m.ID, m.Error.Message)
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
			warnf("signal-cli request %s not delivered: %s", m.ID, res.Type)
		}
	}
}

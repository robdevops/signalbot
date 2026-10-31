// Package signal is the signal-cli client: its JSON-RPC socket, the names
// it knows, mentions, and turning what it receives into messages.
package signal

import (
	"bufio"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// maxLine caps one JSON-RPC frame from signal-cli. Message bodies, mention
// lists and quoted text are all sender-controlled parts of a frame.
const maxLine = 1 << 20

// writeTimeout bounds one request's write, so a stalled socket can't hold
// up every sender.
var writeTimeout = 10 * time.Second

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

// Client is a JSON-RPC client for a signal-cli daemon socket.
type Client struct {
	addr        string
	typingEvery time.Duration
	nextID      atomic.Int64
	mu          sync.Mutex // guards conn and pending, never held over a write
	wmu         sync.Mutex // serialises writes
	conn        net.Conn
	pending     map[string]chan *rpcMsg
	self        atomic.Value // string: the account's ACI, else its number
	known       chan struct{}
	knownOnce   sync.Once
	closed      atomic.Bool // Run has returned
	names       names
}

// New makes a client for the socket at addr: a unix socket path, or
// host:port.
func New(addr string) *Client {
	return &Client{addr: addr, typingEvery: 10 * time.Second, pending: map[string]chan *rpcMsg{},
		known: make(chan struct{}), names: newNames()}
}

// ID is the bot's account ID (ACI, else number), or "" until signal-cli
// says.
func (c *Client) ID() string {
	id, _ := c.self.Load().(string)
	return id
}

// WaitID returns the bot's account ID, waiting for signal-cli to say, or ""
// if ctx ends first.
func (c *Client) WaitID(ctx context.Context) string {
	select {
	case <-c.known:
	case <-ctx.Done():
	}
	return c.ID()
}

// network picks unix for anything that looks like a path ("/run/x",
// "run/x", "./x", or an abstract "@x") and tcp for host:port.
func (c *Client) network() string {
	if _, _, err := net.SplitHostPort(c.addr); err == nil && !strings.ContainsRune(c.addr, '/') && !strings.HasPrefix(c.addr, "@") {
		return "tcp"
	}
	return "unix"
}

// Run connects, learns the bot's own account ID, and calls handle on this
// goroutine for every envelope received. It reconnects with backoff until
// ctx ends. handle must not block.
func (c *Client) Run(ctx context.Context, handle func(*Envelope)) {
	defer c.closed.Store(true)
	backoff := time.Second
	for ctx.Err() == nil {
		conn, err := net.DialTimeout(c.network(), c.addr, 10*time.Second)
		if err != nil {
			logx.Warnf("signal-cli connect failed: %v; retrying in %s", err, backoff)
		} else {
			began := time.Now()
			c.serve(ctx, conn, handle)
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
		backoff = min(2*backoff, 32*time.Second)
	}
}

func (c *Client) serve(ctx context.Context, conn net.Conn, handle func(*Envelope)) {
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	logx.Noticef("connected to signal-cli at %s", c.addr)

	defer context.AfterFunc(ctx, func() { conn.Close() })()
	go func() {
		rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		id, err := c.listAccounts(rctx)
		if err != nil {
			logx.Errorf("listAccounts failed: %v", err)
			conn.Close()
			return
		}
		c.self.Store(id)
		c.knownOnce.Do(func() { close(c.known) })
		logx.Noticef("bot account %s", id)
		c.refreshSoon() // names, on every (re)connect
	}()

	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), maxLine)
	for sc.Scan() {
		c.dispatch(sc.Bytes(), handle)
	}
	if err := sc.Err(); err != nil && ctx.Err() == nil {
		logx.Warnf("signal-cli read failed: %v; reconnecting", err) // a frame over maxLine is bufio.ErrTooLong
	}
	conn.Close()
	c.mu.Lock()
	c.conn = nil
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
}

func (c *Client) dispatch(line []byte, handle func(*Envelope)) {
	defer logx.Recover("reading a message")
	var m rpcMsg
	if err := json.Unmarshal(line, &m); err != nil {
		logx.Warnf("bad JSON from signal-cli (%d bytes), ignored", len(line)) // not err: it can quote the frame
		return
	}
	switch {
	case m.Method == "" && len(m.ID) > 0 && string(m.ID) != "null":
		c.mu.Lock()
		ch := c.pending[string(m.ID)]
		delete(c.pending, string(m.ID))
		c.mu.Unlock()
		if ch != nil {
			ch <- &m
		} else {
			unmatched(&m)
		}
	case m.Method == "receive":
		var p rpcMsg
		if json.Unmarshal(m.Params, &p) != nil || p.Envelope == nil {
			return
		}
		// A group changed (members, name or description): reload the
		// contact list so new members are known and departed ones forgotten.
		if d := p.Envelope.DataMessage; d != nil && d.GroupInfo != nil && d.GroupInfo.Type == "UPDATE" {
			c.refreshSoon()
		}
		handle(p.Envelope)
	}
}

// unmatched reports the outcome of fire-and-forget requests (send,
// sendTyping).
func unmatched(m *rpcMsg) {
	if m.Error != nil {
		logx.Errorf("signal-cli request %s failed: %s", m.ID, txt.Clamp(m.Error.Message, 200))
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
			logx.Warnf("signal-cli request %s not delivered: %s", m.ID, txt.SafeName(res.Type))
		}
	}
}

// write sends one JSON-RPC request. A write that fails or stalls closes the
// connection, so that Run reconnects.
func (c *Client) write(id int64, method string, params any) error {
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		req["params"] = params
	}
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return errors.New("not connected")
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	if _, err = conn.Write(append(b, '\n')); err != nil {
		conn.Close()
	}
	return err
}

// call sends a request and decodes its result into out.
func (c *Client) call(ctx context.Context, method string, params, out any) error {
	id := c.nextID.Add(1)
	key := strconv.FormatInt(id, 10)
	ch := make(chan *rpcMsg, 1)
	c.mu.Lock()
	c.pending[key] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, key); c.mu.Unlock() }()
	if err := c.write(id, method, params); err != nil {
		return err
	}
	select {
	case resp, ok := <-ch:
		if !ok {
			return errors.New("connection closed")
		}
		if resp.Error != nil {
			return fmt.Errorf("rpc error %d: %s", resp.Error.Code, txt.Clamp(resp.Error.Message, 200))
		}
		if json.Unmarshal(resp.Result, out) != nil {
			return errors.New("malformed " + method + " result") // not the error: it can quote the result
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// listAccounts returns the account's ACI, else its number.
func (c *Client) listAccounts(ctx context.Context) (string, error) {
	var accts []struct {
		Number string `json:"number"`
		ACI    string `json:"aci"`
	}
	if err := c.call(ctx, "listAccounts", nil, &accts); err != nil {
		return "", err
	}
	if len(accts) == 0 || cmp.Or(accts[0].ACI, accts[0].Number) == "" {
		return "", errors.New("no account registered")
	}
	return cmp.Or(accts[0].ACI, accts[0].Number), nil
}

func target(chat feature.Chat) map[string]any {
	if chat.GroupID != "" {
		return map[string]any{"groupId": chat.GroupID}
	}
	return map[string]any{"recipient": []string{chat.User}}
}

// Send posts text to chat, with mentions ("start:length:uuid", see
// Mentions) and attachments (data: URIs, see img.Attachment). Whether it was
// delivered is logged when signal-cli says.
func (c *Client) Send(chat feature.Chat, text string, mentions, attachments []string) error {
	p := target(chat)
	p["message"] = text
	if len(mentions) > 0 {
		p["mention"] = mentions
	}
	if len(attachments) > 0 {
		p["attachments"] = attachments
	}
	return c.write(c.nextID.Add(1), "send", p)
}

// Typing shows the bot typing in chat until stop is called.
func (c *Client) Typing(chat feature.Chat) (stop func()) {
	done := make(chan struct{})
	send := func() { _ = c.write(c.nextID.Add(1), "sendTyping", target(chat)) }
	send()
	go func() {
		tick := time.NewTicker(c.typingEvery)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				send()
			}
		}
	}()
	return sync.OnceFunc(func() { close(done) })
}

// data decodes a result that carries a file as base64.
func (c *Client) data(ctx context.Context, method string, params any) ([]byte, error) {
	var r struct {
		Data string `json:"data"`
	}
	if err := c.call(ctx, method, params, &r); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(r.Data)
}

// Attachment fetches an attachment's bytes by its ID, naming the chat it
// arrived in. The bot never reads signal-cli's data directory itself.
func (c *Client) Attachment(ctx context.Context, chat feature.Chat, id string) ([]byte, error) {
	p := map[string]any{"id": id}
	if chat.GroupID != "" {
		p["groupId"] = chat.GroupID
	} else {
		p["recipient"] = chat.User
	}
	return c.data(ctx, "getAttachment", p)
}

// Sticker fetches a sticker's image (WebP, or APNG if animated), which
// signal-cli downloads with the packs of stickers it receives.
func (c *Client) Sticker(ctx context.Context, pack string, id int) ([]byte, error) {
	return c.data(ctx, "getSticker", map[string]any{"packId": pack, "stickerId": id})
}

// StickerLabel describes a sticker by its emoji and its pack's title, e.g.
// `: 🙂 from "Pack"`, or "" if signal-cli doesn't know the pack. Both come
// from whoever made the pack, so they are made safe and the title quoted.
func (c *Client) StickerLabel(ctx context.Context, pack string, id int) string {
	var packs []struct {
		PackID   string `json:"packId"`
		Title    string `json:"title"`
		Stickers []struct {
			ID    int    `json:"id"`
			Emoji string `json:"emoji"`
		} `json:"stickers"`
	}
	if c.call(ctx, "listStickerPacks", nil, &packs) != nil {
		return ""
	}
	for _, p := range packs {
		if p.PackID != pack {
			continue
		}
		label := ""
		for _, s := range p.Stickers {
			if e := txt.Clamp(txt.SafeName(s.Emoji), 8); s.ID == id && e != "" {
				label = ": " + e
			}
		}
		if t := txt.SafeName(p.Title); t != "" {
			label += " from " + strconv.Quote(t)
		}
		return label
	}
	return ""
}

// Group is a group the bot is in.
type Group struct {
	feature.Group
	Members []Member
}

// Member is a group member as signal-cli knows them: an ACI, a number, or
// both.
type Member struct {
	UUID   string `json:"uuid"`
	Number string `json:"number"`
}

// Has reports whether id (an ACI or a number) is a member of g.
func (g Group) Has(id string) bool {
	return id != "" && slices.ContainsFunc(g.Members, func(m Member) bool { return m.UUID == id || m.Number == id })
}

// Groups lists the groups the bot is a member of.
func (c *Client) Groups(ctx context.Context) ([]Group, error) {
	var gs []struct {
		ID       string   `json:"id"`
		Name     string   `json:"name"`
		IsMember bool     `json:"isMember"`
		Members  []Member `json:"members"`
	}
	if err := c.call(ctx, "listGroups", nil, &gs); err != nil {
		return nil, err
	}
	var out []Group
	for _, g := range gs {
		if g.IsMember && g.ID != "" {
			out = append(out, Group{feature.Group{ID: g.ID, Name: txt.SafeName(g.Name)}, g.Members})
		}
	}
	return out, nil
}

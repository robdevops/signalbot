// Package signal is the bot's chat network: a signal-cli client that turns
// what it receives into messages for the core, and sends replies, with the
// names it knows and mentions. Only the main package uses it.
package signal

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/img"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// maxLine caps one JSON-RPC frame from signal-cli, whose bodies, mentions
// and quotes the sender controls: room for a getAttachment result, the
// largest image the bot fetches in base64, and more. A longer frame is
// skipped, and the request it answers fails.
var maxLine = base64.StdEncoding.EncodedLen(img.MaxReceived) + 1<<20

// frameID finds a frame's ID at its end, where signal-cli writes it.
var frameID = regexp.MustCompile(`"id"\s*:\s*(\d+)\s*}\s*$`)

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
	Envelope *envelope `json:"envelope"` // in a receive's params
	tooLong  bool      // the response was skipped, being over maxLine
}

// Client is a JSON-RPC client for a signal-cli daemon socket.
type Client struct {
	addr        string
	allowed     map[string]bool // who may DM the bot, by ACI or number
	typingEvery time.Duration
	nextID      atomic.Int64
	mu          sync.Mutex // guards conn and pending, never held over a write
	wmu         sync.Mutex // serialises writes
	conn        net.Conn
	pending     map[string]chan *rpcMsg
	id          atomic.Value  // string: the account's ACI, else its number
	known       chan struct{} // closed once id is set
	knownOnce   sync.Once
	closed      atomic.Bool // Run has returned
	names       names
	test        *config.Test // nil in production
}

// New makes the client its settings say: SIGNALCLI_SOCKET, a unix socket
// path or host:port, and SIGNAL_DM_USER_IDS, who may DM the bot.
func New(test *config.Test) (*Client, error) {
	addr := config.Env("SIGNALCLI_SOCKET", "")
	if addr == "" {
		return nil, errors.New("SIGNALCLI_SOCKET is required")
	}
	allowed := map[string]bool{}
	// Commas, semicolons or any white space, so a CRLF env file or a tab
	// can't leave an unmatchable ID.
	for _, id := range strings.FieldsFunc(config.Env("SIGNAL_DM_USER_IDS", ""), func(r rune) bool {
		return r == ',' || r == ';' || unicode.IsSpace(r)
	}) {
		allowed[id] = true
	}
	return newClient(addr, allowed, test), nil
}

// newClient is New with its settings given.
func newClient(addr string, allowed map[string]bool, test *config.Test) *Client {
	c := &Client{addr: addr, allowed: allowed, typingEvery: 10 * time.Second, pending: map[string]chan *rpcMsg{},
		known: make(chan struct{}), test: test}
	c.names.byID, c.names.numbers, c.names.minRefresh = map[string]rankedName{}, map[string]string{}, 15*time.Second
	if test != nil && test.ReloadGap > 0 {
		c.names.minRefresh = test.ReloadGap
	}
	return c
}

// self is the bot's account ID, or "" until signal-cli says.
func (c *Client) self() string {
	id, _ := c.id.Load().(string)
	return id
}

// WaitID returns the bot's account ID, waiting for signal-cli to say, or ""
// if ctx ends first.
func (c *Client) WaitID(ctx context.Context) string {
	select {
	case <-c.known:
	case <-ctx.Done():
	}
	return c.self()
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
// goroutine for every message received that the core may act on. It
// reconnects with backoff until ctx ends. handle must not block.
func (c *Client) Run(ctx context.Context, handle func(bot.Received)) {
	defer c.closed.Store(true)
	backoff := time.Second
	for ctx.Err() == nil {
		conn, err := net.DialTimeout(c.network(), c.addr, 10*time.Second)
		if err != nil {
			logx.Warnf("signal-cli connect failed: %v; retrying in %s", err, backoff)
		} else {
			began := time.Now()
			c.serve(ctx, conn, handle)
			// Only a connection that held resets it, so one that dies at once can't spin.
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

func (c *Client) serve(ctx context.Context, conn net.Conn, handle func(bot.Received)) {
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
		c.id.Store(id)
		c.knownOnce.Do(func() { close(c.known) })
		logx.Noticef("bot account %s", id)
		c.refreshSoon() // names, on every (re)connect
	}()

	err := readFrames(conn, maxLine, func(line []byte) { c.dispatch(line, handle) }, c.skipped)
	if err != nil && !errors.Is(err, io.EOF) && ctx.Err() == nil {
		logx.Warnf("signal-cli read failed: %v; reconnecting", err)
	}
	conn.Close()
	c.mu.Lock()
	for _, ch := range c.pending {
		close(ch)
	}
	c.conn, c.pending = nil, map[string]chan *rpcMsg{}
	c.mu.Unlock()
}

// readFrames calls frame with each line r holds, without its newline, and
// skipped with the last bytes of each line longer than max, which it
// skips, until r fails. frame's line is reused after it returns.
func readFrames(r io.Reader, max int, frame func([]byte), skipped func(tail []byte)) error {
	br := bufio.NewReaderSize(r, 64<<10)
	var line, tail []byte
	skipping := false
	for {
		chunk, err := br.ReadSlice('\n')
		if err != nil && err != bufio.ErrBufferFull {
			return err
		}
		if !skipping && len(line)+len(chunk) > max+1 { // +1: the newline
			skipping, line = true, nil
		}
		if skipping {
			tail = last(append(tail, last(chunk, 256)...), 256)
		} else {
			line = append(line, chunk...)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if skipping {
			skipped(tail)
			skipping, tail = false, tail[:0]
		} else {
			frame(bytes.TrimSuffix(line, []byte("\n")))
			line = line[:0]
			if cap(line) > 1<<20 {
				line = nil // don't hold on to a big image's frame
			}
		}
	}
}

// last is b's last n bytes, or b if shorter.
func last(b []byte, n int) []byte { return b[max(0, len(b)-n):] }

// skipped fails the request a frame over maxLine answers, if its ID is
// found in tail, the frame's end.
func (c *Client) skipped(tail []byte) {
	logx.Warnf("signal-cli sent a frame over %d MiB, skipped", maxLine>>20)
	m := frameID.FindSubmatch(tail)
	if m == nil {
		return
	}
	c.mu.Lock()
	ch := c.pending[string(m[1])]
	delete(c.pending, string(m[1]))
	c.mu.Unlock()
	if ch != nil {
		ch <- &rpcMsg{tooLong: true}
	}
}

func (c *Client) dispatch(line []byte, handle func(bot.Received)) {
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
		if r, ok := c.event(p.Envelope); ok {
			handle(r)
		}
		if c.test != nil && c.test.Handled != nil {
			c.test.Handled()
		}
	}
}

// unmatched logs the outcome of a fire-and-forget request, send or sendTyping.
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
	b, _ := json.Marshal(req) // plain values, which always encode
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return errors.New("not connected")
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	conn.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err := conn.Write(append(b, '\n'))
	if err != nil {
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
		if resp.tooLong {
			return errors.New(method + " result too large")
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

// Media fetches msg.Media[i] from signal-cli, waiting at most 15s: an image
// by its attachment ID and chat, or a sticker's image (WebP, or APNG if
// animated), which signal-cli downloads with the packs it receives. The
// bot never reads signal-cli's data directory itself.
func (c *Client) Media(ctx context.Context, msg feature.Message, i int) ([]byte, error) {
	if i < 0 || i >= len(msg.Media) {
		return nil, errors.New("no such media")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	m := msg.Media[i]
	var r struct {
		Data []byte `json:"data"` // base64, decoded as it is read: one copy fewer of a big image
	}
	var err error
	switch {
	case m.Sticker:
		err = c.call(ctx, "getSticker", stickerOf(m.ID), &r) // {"packId", "stickerId"}
	case msg.Chat.GroupID != "":
		err = c.call(ctx, "getAttachment", map[string]any{"id": m.ID, "groupId": msg.Chat.GroupID}, &r)
	default:
		err = c.call(ctx, "getAttachment", map[string]any{"id": m.ID, "recipient": msg.Chat.User}, &r)
	}
	if err != nil {
		return nil, err
	}
	return r.Data, nil
}

// stickerLabel describes a sticker by its emoji and its pack's title, e.g.
// `: 🙂 from "Pack"`, or "" if signal-cli doesn't know the pack, waiting at
// most 10s. The pack's maker chose both, so they are made safe.
func (c *Client) stickerLabel(ctx context.Context, s sticker) string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
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
		if p.PackID != s.PackID {
			continue
		}
		label := ""
		for _, st := range p.Stickers {
			if e := txt.Clamp(txt.SafeName(st.Emoji), 8); st.ID == s.StickerID && e != "" {
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

// group is a group the bot is in, with its members by ACI, number or both.
type group struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	IsMember bool     `json:"isMember"`
	Members  []member `json:"members"`
}

type member struct {
	UUID   string `json:"uuid"`
	Number string `json:"number"`
}

// groups lists the groups the bot shares with user (an ACI or a number).
func (c *Client) groups(ctx context.Context, user string) ([]group, error) {
	var gs []group
	if err := c.call(ctx, "listGroups", nil, &gs); err != nil {
		return nil, err
	}
	return slices.DeleteFunc(gs, func(g group) bool {
		return !g.IsMember || g.ID == "" || !slices.ContainsFunc(g.Members, func(m member) bool { return is(user, m.UUID, m.Number) })
	}), nil
}

// Groups lists the groups the bot shares with user (an ACI or a number).
func (c *Client) Groups(ctx context.Context, user string) ([]feature.Group, error) {
	gs, err := c.groups(ctx, user)
	var out []feature.Group
	for _, g := range gs {
		out = append(out, feature.Group{ID: g.ID, Name: txt.SafeName(g.Name)})
	}
	return out, err
}

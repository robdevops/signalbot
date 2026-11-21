package addon

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// client plays the bot: it writes requests to a Server's stdin and reads
// what the Server writes to stdout, which must be JSON-RPC lines alone.
type client struct {
	t      *testing.T
	in     *io.PipeWriter
	lines  chan json.RawMessage // every line the server wrote
	nextID int
	done   chan error // Run's result
}

func start(t *testing.T, s *Server) *client {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &client{t: t, in: inW, lines: make(chan json.RawMessage, 64), done: make(chan error, 1)}
	go func() { c.done <- s.Run(context.Background(), inR, outW); outW.Close() }()
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(nil, 4<<20)
		for sc.Scan() {
			line := json.RawMessage(append([]byte(nil), sc.Bytes()...))
			if !json.Valid(line) {
				t.Errorf("stdout carried something not JSON: %q", line)
			}
			c.lines <- line
		}
		close(c.lines)
	}()
	t.Cleanup(func() {
		inW.Close()
		select {
		case err := <-c.done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("Run didn't return when stdin closed")
		}
	})
	return c
}

func (c *client) send(v any) {
	c.t.Helper()
	data, _ := json.Marshal(v)
	if _, err := c.in.Write(append(data, '\n')); err != nil {
		c.t.Fatal(err)
	}
}

// request sends a request and returns its ID.
func (c *client) request(method string, params any) int {
	c.nextID++
	c.send(map[string]any{"jsonrpc": "2.0", "id": c.nextID, "method": method, "params": params})
	return c.nextID
}

// reply is the next message for id, skipping notifications, which it
// returns too.
func (c *client) reply(id int) (resp struct {
	Result json.RawMessage
	Error  *rpcError
}, notes []string) {
	c.t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case line, ok := <-c.lines:
			if !ok {
				c.t.Fatal("stdout closed")
			}
			var m struct {
				ID     *int
				Method string
				Result json.RawMessage
				Error  *rpcError
			}
			json.Unmarshal(line, &m)
			if m.ID == nil {
				notes = append(notes, m.Method)
				continue
			}
			if *m.ID != id {
				c.t.Fatalf("reply to %d while waiting for %d", *m.ID, id)
			}
			resp.Result, resp.Error = m.Result, m.Error
			return resp, notes
		case <-timeout:
			c.t.Fatalf("no reply to %d", id)
		}
	}
}

// result asks and decodes the result into v.
func (c *client) result(method string, params, v any) {
	c.t.Helper()
	resp, _ := c.reply(c.request(method, params))
	if resp.Error != nil {
		c.t.Fatalf("%s: %v", method, resp.Error.Message)
	}
	if v != nil {
		if err := json.Unmarshal(resp.Result, v); err != nil {
			c.t.Fatalf("%s: %v", method, err)
		}
	}
}

// note waits for a notification.
func (c *client) note(method string) {
	c.t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case line := <-c.lines:
			var m struct{ Method string }
			json.Unmarshal(line, &m)
			if m.Method == method {
				return
			}
		case <-timeout:
			c.t.Fatalf("no %s", method)
		}
	}
}

func initialize(c *client, network string) map[string]any {
	var r map[string]any
	c.result("initialize", map[string]any{
		"protocolVersion": Protocol,
		"capabilities":    map[string]any{"experimental": map[string]any{KeyAddon: Capability{Version: Version, Network: network}}},
	}, &r)
	return r
}

func echoTool() Tool {
	return Tool{
		Name: "echo", Description: "says it back", Visibility: []string{"bot"},
		Schema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
		Call: func(_ context.Context, c *Context, raw json.RawMessage) (Result, error) {
			var a struct{ Text string }
			if err := DecodeArgs(raw, &a); err != nil {
				return Result{}, err
			}
			who := "the model"
			if c != nil {
				who = c.Sender + " in " + c.Chat
			}
			return Result{Content: []Content{{Type: "text", Text: a.Text + " from " + who}}, Meta: &ResultMeta{React: "👍"}}, nil
		},
	}
}

func TestInitializeAndTools(t *testing.T) {
	s := New(Config{Name: "game", Title: "Game", Version: "1", Tools: []Tool{echoTool(), {Name: "both", Visibility: []string{"model", "bot"}}, {Name: "plain"}}})
	c := start(t, s)
	r := initialize(c, "signal")
	caps, _ := json.Marshal(r["capabilities"])
	if r["protocolVersion"] != Protocol || !strings.Contains(string(caps), `"`+KeyAddon+`":{"version":1}`) || !strings.Contains(string(caps), `"subscribe":true`) {
		t.Fatalf("initialize %v", r)
	}
	if info, _ := json.Marshal(r["serverInfo"]); string(info) != `{"name":"game","title":"Game","version":"1"}` {
		t.Errorf("serverInfo %s", info)
	}
	if s.Network() != "signal" {
		t.Errorf("network %q", s.Network())
	}
	var list struct {
		Tools []struct {
			Name        string
			InputSchema json.RawMessage
			Meta        map[string][]string `json:"_meta"`
		}
	}
	c.result("tools/list", struct{}{}, &list)
	if len(list.Tools) != 3 {
		t.Fatalf("tools %+v", list.Tools)
	}
	if got := list.Tools[0].Meta[KeyVisibility]; len(got) != 1 || got[0] != "bot" || !strings.Contains(string(list.Tools[0].InputSchema), `"text"`) {
		t.Errorf("echo %+v", list.Tools[0])
	}
	if list.Tools[2].Meta != nil || string(list.Tools[2].InputSchema) != `{"type":"object"}` {
		t.Errorf("plain %+v", list.Tools[2])
	}
	c.result("ping", struct{}{}, nil)
	resp, _ := c.reply(c.request("nope", struct{}{}))
	if resp.Error == nil || resp.Error.Code != codeNoMethod {
		t.Errorf("unknown method %+v", resp)
	}
}

func TestCall(t *testing.T) {
	c := start(t, New(Config{Name: "game", Tools: []Tool{echoTool()}}))
	initialize(c, "")
	var r Result
	c.result("tools/call", map[string]any{"name": "echo", "arguments": map[string]string{"text": "hi"},
		"_meta": map[string]any{KeyContext: Context{Chat: "c_one", Sender: "u_alice", Addressed: true, Group: true, Sent: 100}}}, &r)
	if len(r.Content) != 1 || r.Content[0].Text != "hi from u_alice in c_one" || r.Meta == nil || r.Meta.React != "👍" {
		t.Errorf("with context %+v", r)
	}
	c.result("tools/call", map[string]any{"name": "echo", "arguments": map[string]string{"text": "hi"}}, &r)
	if r.Content[0].Text != "hi from the model" {
		t.Errorf("without context %+v", r)
	}
	for name, params := range map[string]any{
		"unknown tool": map[string]any{"name": "nope"},
		"bad args":     map[string]any{"name": "echo", "arguments": "not an object"},
	} {
		resp, _ := c.reply(c.request("tools/call", params))
		if resp.Error == nil || resp.Error.Code != codeInvalidParams {
			t.Errorf("%s: %+v", name, resp)
		}
	}
}

// A call the bot gives up on is cancelled: its context ends.
func TestCancel(t *testing.T) {
	ended := make(chan error, 1)
	slow := Tool{Name: "slow", Call: func(ctx context.Context, _ *Context, _ json.RawMessage) (Result, error) {
		<-ctx.Done()
		ended <- ctx.Err()
		return Text("too late"), nil
	}}
	c := start(t, New(Config{Name: "game", Tools: []Tool{slow}}))
	id := c.request("tools/call", map[string]any{"name": "slow"})
	c.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": id}})
	select {
	case err := <-ended:
		if err != context.Canceled {
			t.Errorf("ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the call wasn't cancelled")
	}
	c.reply(id)
}

// The outbox: a subscribed bot hears of new ops, a read gets them with a
// cursor, a read at that cursor lets go of them, and a cursor the server
// doesn't know gets everything still held.
func TestOutbox(t *testing.T) {
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	changes := 0
	s := New(Config{Name: "game", Now: func() time.Time { return now }, Changed: func() { mu.Lock(); changes++; mu.Unlock() }})
	c := start(t, s)
	initialize(c, "")
	c.result("resources/subscribe", uriParams{OutboxURI}, nil)
	s.Queue(Op{Op: "post", Chat: "c_one", Text: "{u_alice} wins"}, Op{Op: "unwatch", Chat: "c_one", Watch: "r1"})
	c.note("notifications/resources/updated")

	read := func(cursor string) Page {
		var r struct {
			Contents []resourceContent
		}
		c.result("resources/read", uriParams{OutboxURI + "/" + cursor}, &r)
		var p Page
		if len(r.Contents) != 1 || json.Unmarshal([]byte(r.Contents[0].Text), &p) != nil {
			t.Fatalf("read %+v", r)
		}
		return p
	}
	p := read("0")
	if len(p.Ops) != 2 || p.Ops[0].Text != "{u_alice} wins" || p.Ops[1].Watch != "r1" || p.More || !strings.HasSuffix(p.Ops[1].ID, ".2") {
		t.Fatalf("first page %+v", p)
	}
	if len(s.Outbox()) != 2 {
		t.Fatal("ops let go of before the bot read past them")
	}
	if p2 := read(p.Cursor); len(p2.Ops) != 0 || p2.Cursor != p.Cursor || len(s.Outbox()) != 0 {
		t.Fatalf("after the cursor %+v, outbox %v", p2, s.Outbox())
	}
	mu.Lock()
	if changes != 1 {
		t.Errorf("Changed called %d times", changes)
	}
	mu.Unlock()

	s.Queue(Op{Op: "post", Chat: "c_one", Text: "again"})
	if p := read("other.5"); len(p.Ops) != 1 || p.Ops[0].Text != "again" {
		t.Errorf("an unknown cursor %+v", p)
	}
	resp, _ := c.reply(c.request("resources/read", uriParams{"signalbot://elsewhere"}))
	if resp.Error == nil || resp.Error.Code != codeNotFound {
		t.Errorf("another resource %+v", resp)
	}
}

// Ops saved before a restart come back with their IDs, and a new run's
// first read, whatever its cursor, gets them; ones held past keepOps go.
func TestRestore(t *testing.T) {
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	s := New(Config{Name: "game", Now: func() time.Time { return now }})
	s.Restore([]Held{
		{Op: Op{ID: "old.7", Op: "post", Chat: "c_one", Text: "results"}, Queued: now.Add(-time.Minute)},
		{Op: Op{ID: "old.8", Op: "post", Chat: "c_one", Text: "stale"}, Queued: now.Add(-2 * time.Hour)},
	})
	c := start(t, s)
	initialize(c, "")
	var r struct{ Contents []resourceContent }
	c.result("resources/read", uriParams{OutboxURI + "/old.8"}, &r)
	var p Page
	json.Unmarshal([]byte(r.Contents[0].Text), &p)
	if len(p.Ops) != 1 || p.Ops[0].ID != "old.7" {
		t.Errorf("after a restart %+v", p)
	}
	if strings.HasPrefix(p.Cursor, "old.") {
		t.Errorf("cursor %q is the old run's", p.Cursor)
	}
}

// A long page is split, at least one op a page.
func TestOutboxPages(t *testing.T) {
	s := New(Config{Name: "game"})
	big := strings.Repeat("x", pageSize/2)
	for i := range 3 {
		s.Queue(Op{Op: "post", Chat: "c_one", Text: big + strconv.Itoa(i)})
	}
	c := start(t, s)
	initialize(c, "")
	var r struct{ Contents []resourceContent }
	c.result("resources/read", uriParams{OutboxURI + "/0"}, &r)
	var p Page
	json.Unmarshal([]byte(r.Contents[0].Text), &p)
	if len(p.Ops) != 1 || !p.More {
		t.Errorf("%d ops, more %v", len(p.Ops), p.More)
	}
}

// Garbage on stdin gets a parse error, and Run carries on.
func TestBadLine(t *testing.T) {
	c := start(t, New(Config{Name: "game"}))
	c.in.Write([]byte("not json\n"))
	line := <-c.lines
	if !strings.Contains(string(line), `"code":-32700`) {
		t.Errorf("got %s", line)
	}
	c.result("ping", struct{}{}, nil)
}

// Queue refuses an op that could never reach the bot, and queues none of
// the ops given with it.
func TestQueueLimits(t *testing.T) {
	s := New(Config{Name: "game"})
	img := Image{Data: "aGk=", MimeType: "image/png"}
	if err := s.Queue(Op{Op: "post", Chat: "c_one", Text: "ok"}, Op{Op: "post", Chat: "c_one", Images: []Image{img, img, img}}); err == nil {
		t.Error("3 images queued")
	}
	if err := s.Queue(Op{Op: "post", Chat: "c_one", Text: strings.Repeat(`"`, pageSize/2)}); err == nil {
		t.Error("an op too big once escaped was queued")
	}
	if n := len(s.Outbox()); n != 0 {
		t.Fatalf("%d ops queued", n)
	}
	if err := s.Queue(Op{Op: "post", Chat: "c_one", Images: []Image{img, img}}); err != nil {
		t.Errorf("2 images: %v", err)
	}
}

// A page of ops full of characters JSON escapes still fits the bot's
// 4 MiB line.
func TestPageFitsALine(t *testing.T) {
	s := New(Config{Name: "game"})
	for range 1000 {
		if err := s.Queue(Op{Op: "post", Chat: "c_one", Text: strings.Repeat(`"\`, 1500)}); err != nil {
			t.Fatal(err)
		}
	}
	c := start(t, s)
	initialize(c, "")
	id := c.request("resources/read", uriParams{OutboxURI + "/0"})
	for line := range c.lines {
		if len(line) >= 4<<20 {
			t.Fatalf("a %d-byte line", len(line))
		}
		var m struct{ ID *int }
		if json.Unmarshal(line, &m) == nil && m.ID != nil && *m.ID == id {
			return
		}
	}
	t.Fatal("no reply")
}

// Run returns when its context ends, and cancels the calls in flight; when
// stdin ends it cancels them too before it returns.
func TestRunStops(t *testing.T) {
	for _, how := range []string{"ctx", "eof"} {
		ended := make(chan struct{})
		slow := Tool{Name: "slow", Call: func(ctx context.Context, _ *Context, _ json.RawMessage) (Result, error) {
			<-ctx.Done()
			close(ended)
			return Text("stopped"), nil
		}}
		s := New(Config{Name: "game", Tools: []Tool{slow}})
		inR, inW := io.Pipe()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.Run(ctx, inR, io.Discard) }()
		inW.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"slow"}}` + "\n"))
		time.Sleep(50 * time.Millisecond)
		if how == "ctx" {
			cancel()
		} else {
			inW.Close()
		}
		select {
		case err := <-done:
			if how == "ctx" && err != context.Canceled || how == "eof" && err != nil {
				t.Errorf("%s: Run returned %v", how, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: Run didn't return", how)
		}
		select {
		case <-ended:
		default:
			t.Errorf("%s: the call in flight wasn't cancelled before Run returned", how)
		}
		cancel()
		inW.Close()
	}
}

// A result with no content still sends an array, as MCP requires.
func TestEmptyResult(t *testing.T) {
	empty := Tool{Name: "empty", Call: func(context.Context, *Context, json.RawMessage) (Result, error) { return Result{}, nil }}
	c := start(t, New(Config{Name: "game", Tools: []Tool{empty}}))
	resp, _ := c.reply(c.request("tools/call", map[string]any{"name": "empty"}))
	if !strings.Contains(string(resp.Result), `"content":[]`) {
		t.Errorf("result %s", resp.Result)
	}
}

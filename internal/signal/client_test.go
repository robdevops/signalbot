package signal

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

const (
	botID   = "00000000-0000-4000-8000-000000000000"
	aliceID = "11111111-1111-4111-8111-111111111111"
	bobID   = "22222222-2222-4222-8222-222222222222"
	groupID = "Z3JvdXA="
)

// fake is a signal-cli daemon on a unix socket. It answers each method that
// has a result and records every request.
type fake struct {
	sock  string
	conns chan net.Conn
	mu    sync.Mutex
	res   map[string]string // method: result JSON
	reqs  []request
}

type request struct {
	Method string
	Params map[string]any
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{sock: filepath.Join(t.TempDir(), "s.sock"), conns: make(chan net.Conn, 4),
		res: map[string]string{"listAccounts": `[{"number":"+15550000","aci":"` + botID + `"}]`, "listContacts": `[]`}}
	ln, err := net.Listen("unix", f.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.conns <- c
			go f.serve(c)
		}
	}()
	return f
}

func (f *fake) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		json.Unmarshal(line, &req)
		f.mu.Lock()
		f.reqs = append(f.reqs, request{req.Method, req.Params})
		res, ok := f.res[req.Method]
		f.mu.Unlock()
		if ok {
			fmt.Fprintf(c, `{"jsonrpc":"2.0","result":%s,"id":%s}`+"\n", res, req.ID)
		}
	}
}

func (f *fake) set(method, result string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.res[method] = result
}

func (f *fake) calls(method string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, r := range f.reqs {
		if r.Method == method {
			out = append(out, r.Params)
		}
	}
	return out
}

func receive(c net.Conn, envelope string) {
	fmt.Fprintf(c, `{"jsonrpc":"2.0","method":"receive","params":{"envelope":%s}}`+"\n", envelope)
}

// run starts c on f and returns the envelopes it hands on and the server
// side of the first connection. Run has returned by the time the test ends.
func run(t *testing.T, c *Client, f *fake) (<-chan *Envelope, net.Conn) {
	t.Helper()
	got := make(chan *Envelope, 16)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx, func(e *Envelope) { got <- e }) }()
	t.Cleanup(func() { cancel(); <-done })
	wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
	defer wcancel()
	if id := c.WaitID(wctx); id != botID {
		t.Fatalf("ID %q", id)
	}
	return got, <-f.conns
}

func captureLogs(t *testing.T) *logx.Buffer {
	t.Helper()
	var buf logx.Buffer
	t.Cleanup(logx.SetOutput(&buf, &buf, false))
	return &buf
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func next(t *testing.T, got <-chan *Envelope) *Envelope {
	t.Helper()
	select {
	case e := <-got:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("no envelope")
		return nil
	}
}

// The client learns its account, hands on what it receives, and connects
// again when the socket drops.
func TestReceiveAndReconnect(t *testing.T) {
	captureLogs(t)
	f := newFake(t)
	c := New(f.sock)
	got, conn := run(t, c, f)
	receive(conn, `{"sourceUuid":"`+aliceID+`","dataMessage":{"message":"hi"}}`)
	if e := next(t, got); e.SourceUUID != aliceID || e.DataMessage.Message != "hi" {
		t.Fatalf("got %+v", e)
	}
	conn.Close()
	conn2 := <-f.conns
	receive(conn2, `{"sourceUuid":"`+bobID+`","dataMessage":{"message":"again"}}`)
	if e := next(t, got); e.SourceUUID != bobID {
		t.Fatalf("after reconnecting got %+v", e)
	}
}

// A write that stalls times out and drops the connection, so the client
// connects again; the reader keeps handing on messages meanwhile.
func TestStalledWriteReconnects(t *testing.T) {
	captureLogs(t)
	defer func(d time.Duration) { writeTimeout = d }(writeTimeout)
	writeTimeout = time.Second
	sock := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	conns := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns <- c
		}
	}()
	c := New(sock)
	got := make(chan *Envelope, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx, func(e *Envelope) { got <- e }) }()
	defer func() { cancel(); <-done }()

	srv := <-conns
	defer srv.Close()
	var req struct{ ID json.RawMessage }
	line, _ := bufio.NewReader(srv).ReadBytes('\n') // listAccounts; then it stops reading
	json.Unmarshal(line, &req)
	fmt.Fprintf(srv, `{"jsonrpc":"2.0","result":[{"aci":"%s"}],"id":%s}`+"\n", botID, req.ID)
	wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
	defer wcancel()
	if c.WaitID(wctx) != botID {
		t.Fatal("no account")
	}
	sent := make(chan error)
	go func() { sent <- c.Send(feature.Chat{User: aliceID}, strings.Repeat("x", 8<<20), nil, nil) }()
	receive(srv, `{"sourceUuid":"`+aliceID+`","dataMessage":{"message":"hi"}}`)
	next(t, got)
	if err := <-sent; err == nil {
		t.Fatal("a stalled write succeeded")
	}
	select {
	case c2 := <-conns:
		c2.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("no reconnect")
	}
}

func TestSendAndTyping(t *testing.T) {
	captureLogs(t)
	f := newFake(t)
	c := New(f.sock)
	c.typingEvery = 20 * time.Millisecond
	run(t, c, f)
	group, dm := feature.Chat{GroupID: groupID}, feature.Chat{User: aliceID}
	if err := c.Send(group, "hi @Alice", []string{"3:6:" + aliceID}, []string{"data:image/png;base64,AA=="}); err != nil {
		t.Fatal(err)
	}
	if err := c.Send(dm, "plain", nil, nil); err != nil {
		t.Fatal(err)
	}
	eventually(t, "two sends", func() bool { return len(f.calls("send")) == 2 })
	s := f.calls("send")
	if s[0]["groupId"] != groupID || s[0]["message"] != "hi @Alice" || fmt.Sprint(s[0]["mention"]) != "[3:6:"+aliceID+"]" ||
		fmt.Sprint(s[0]["attachments"]) != "[data:image/png;base64,AA==]" {
		t.Errorf("group send %v", s[0])
	}
	if fmt.Sprint(s[1]["recipient"]) != "["+aliceID+"]" || s[1]["mention"] != nil || s[1]["attachments"] != nil {
		t.Errorf("DM send %v", s[1])
	}

	stop := c.Typing(dm)
	eventually(t, "typing repeated", func() bool { return len(f.calls("sendTyping")) >= 3 })
	stop()
	stop() // twice is fine
	time.Sleep(50 * time.Millisecond)
	n := len(f.calls("sendTyping"))
	time.Sleep(60 * time.Millisecond)
	if m := len(f.calls("sendTyping")); m != n {
		t.Errorf("typing went on after stop: %d then %d", n, m)
	}
}

func TestUnmatchedAndBadFramesAreLogged(t *testing.T) {
	for _, tc := range []struct{ name, line, want string }{
		{"rpc error", `{"jsonrpc":"2.0","error":{"code":-1,"message":"Failed to send message"},"id":7}`, "Failed to send message"},
		{"not delivered", `{"jsonrpc":"2.0","result":{"results":[{"type":"NETWORK_FAILURE"}]},"id":8}`, "NETWORK_FAILURE"},
		{"delivered is quiet", `{"jsonrpc":"2.0","result":{"results":[{"type":"SUCCESS"}]},"id":9}`, ""},
		{"typing is quiet", `{"jsonrpc":"2.0","result":{"timestamp":1},"id":10}`, ""},
		{"bad JSON, not quoted", `{"secret text`, "bad JSON from signal-cli (13 bytes)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			New("x").dispatch([]byte(tc.line), func(*Envelope) { t.Fatal("unexpected envelope") })
			out := logs.String()
			if tc.want == "" && out != "" || !strings.Contains(out, tc.want) || strings.Contains(out, "secret") {
				t.Fatalf("logged %q, want %q", out, tc.want)
			}
		})
	}
}

// A panic while handing on a message is logged without its value, and the
// reader carries on.
func TestDispatchRecovers(t *testing.T) {
	logs := captureLogs(t)
	New("x").dispatch([]byte(`{"method":"receive","params":{"envelope":{}}}`), func(*Envelope) { panic("secret text") })
	if out := logs.String(); !strings.Contains(out, "internal error") || strings.Contains(out, "secret") {
		t.Fatalf("logged %q", out)
	}
}

func TestFrameTooLong(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	_, conn := run(t, New(f.sock), f)
	go fmt.Fprintf(conn, `{"x":"%s"}`+"\n", strings.Repeat("a", maxLine))
	eventually(t, "the warning", func() bool { return strings.Contains(logs.String(), "signal-cli read failed") })
}

func TestFilesStickersAndGroups(t *testing.T) {
	captureLogs(t)
	f := newFake(t)
	c := New(f.sock)
	run(t, c, f)
	ctx := context.Background()
	f.set("getAttachment", `{"data":"`+base64.StdEncoding.EncodeToString([]byte("PNG"))+`"}`)
	if b, err := c.Attachment(ctx, feature.Chat{GroupID: groupID}, "att1"); err != nil || string(b) != "PNG" {
		t.Fatalf("attachment %q %v", b, err)
	}
	if _, err := c.Attachment(ctx, feature.Chat{User: aliceID}, "att2"); err != nil {
		t.Fatal(err)
	}
	a := f.calls("getAttachment")
	if a[0]["id"] != "att1" || a[0]["groupId"] != groupID || a[1]["recipient"] != aliceID {
		t.Errorf("getAttachment params %v", a)
	}

	f.set("getSticker", `{"data":"`+base64.StdEncoding.EncodeToString([]byte("WEBP"))+`"}`)
	if b, err := c.Sticker(ctx, "pack1", 3); err != nil || string(b) != "WEBP" {
		t.Fatalf("sticker %q %v", b, err)
	}
	if p := f.calls("getSticker")[0]; p["packId"] != "pack1" || p["stickerId"] != float64(3) {
		t.Errorf("getSticker params %v", p)
	}
	f.set("listStickerPacks", `[{"packId":"pack1","title":"Alice's\npack","stickers":[{"id":3,"emoji":"🙂"}]}]`)
	if got := c.StickerLabel(ctx, "pack1", 3); got != `: 🙂 from "Alice's pack"` {
		t.Errorf("label %q", got)
	}
	if got := c.StickerLabel(ctx, "other", 3); got != "" {
		t.Errorf("unknown pack labelled %q", got)
	}

	f.set("listGroups", `[{"id":"`+groupID+`","name":"Family","isMember":true,"members":[{"number":"+1555","uuid":"`+aliceID+`"},{"number":null,"uuid":"`+bobID+`"}]},`+
		`{"id":"left","name":"Old","isMember":false,"members":[]}]`)
	gs, err := c.Groups(ctx)
	if err != nil || len(gs) != 1 || gs[0].ID != groupID || gs[0].Name != "Family" ||
		!gs[0].Has(aliceID) || !gs[0].Has("+1555") || !gs[0].Has(bobID) || gs[0].Has("") || gs[0].Has("+1999") {
		t.Fatalf("groups %+v %v", gs, err)
	}

	f.set("listGroups", `{"error":"x"`) // not even JSON: the call times out
	tctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	if _, err := c.Groups(tctx); err == nil {
		t.Error("no error")
	}
}

func TestCallsFailWhenNotConnected(t *testing.T) {
	c := New("x")
	if err := c.Send(feature.Chat{User: aliceID}, "hi", nil, nil); err == nil {
		t.Error("send while not connected")
	}
	if _, err := c.Groups(context.Background()); err == nil {
		t.Error("call while not connected")
	}
}

func TestNetwork(t *testing.T) {
	for addr, want := range map[string]string{"/run/s": "unix", "./s": "unix", "@abs": "unix", "localhost:7583": "tcp", "[::1]:7583": "tcp"} {
		if got := New(addr).network(); got != want {
			t.Errorf("%s: %s", addr, got)
		}
	}
}

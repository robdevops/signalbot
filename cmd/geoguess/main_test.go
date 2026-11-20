package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robdevops/signalbot/cmd/geoguess/geo"
)

// fakeClock is a clock whose time moves only when a test says.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	at      time.Time
	f       func()
	stopped bool
}

func (t *fakeTimer) Stop() bool {
	was := !t.stopped
	t.stopped = true
	return was
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) AfterFunc(d time.Duration, f func()) stopper {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{at: c.now.Add(d), f: f}
	c.timers = append(c.timers, t)
	return t
}

// Advance moves the time on by d, firing the timers then due in order.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []*fakeTimer
	c.timers = slices.DeleteFunc(c.timers, func(t *fakeTimer) bool {
		if !t.at.After(c.now) {
			due = append(due, t)
			return true
		}
		return false
	})
	c.mu.Unlock()
	slices.SortStableFunc(due, func(a, b *fakeTimer) int { return a.at.Compare(b.at) })
	for _, t := range due {
		if !t.stopped {
			t.f()
		}
	}
}

// The photos the fake finder offers: one in Ballarat, and one in Paris for
// a round in Europe.
var (
	ballarat = geo.Point{Lat: -37.5622, Lon: 143.8503}
	paris    = geo.Point{Lat: 48.8566, Lon: 2.3522}
)

// syncBuffer is a log a test can read while the server may write it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// fakeFinder finds a photo at once, or never if off.
type fakeFinder struct {
	mu  sync.Mutex
	n   int
	off bool
}

func (f *fakeFinder) Find(ctx context.Context, regions string) (geo.Photo, error) {
	f.mu.Lock()
	f.n++
	n, off := f.n, f.off
	f.mu.Unlock()
	if off {
		<-ctx.Done()
		return geo.Photo{}, ctx.Err()
	}
	p := geo.Photo{
		Candidate: geo.Candidate{
			ID: strconv.Itoa(n), Source: "Mapillary", Creator: "alice", Licence: "CC BY-SA 4.0",
			Link: "https://www.mapillary.com/app/?pKey=" + strconv.Itoa(n), At: ballarat,
		},
		Place: geo.Place{Name: "Ballarat, Victoria, Australia", Country: "Australia", CountryCode: "AU"},
		JPEG:  []byte("jpeg " + strconv.Itoa(n)),
	}
	if strings.Contains(strings.ToLower(regions), "europe") {
		p.At, p.Place = paris, geo.Place{Name: "Paris, Île-de-France, France", Country: "France", CountryCode: "FR"}
	}
	return p, nil
}

// client drives a server over pipes, as signalbot would, and checks that
// every line the server writes is a valid JSON-RPC message.
type client struct {
	t      *testing.T
	s      *server
	in     *io.PipeWriter
	notes  chan string
	served chan error
	done   sync.WaitGroup // the reader and the prefetcher
	stop   context.CancelFunc

	mu      sync.Mutex
	id      int
	waiting map[string]chan response
	lines   int
}

// rpcIn is a message as the client reads it.
type rpcIn struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
}

// testServer is a server keeping its state in dir, on clk, with photos from
// the fake finder, and its log in logs.
func testServer(t *testing.T, dir string, clk clock, photos finder, logs io.Writer) *server {
	t.Helper()
	cfg, err := loadConfig(func(k string) string { return map[string]string{"GEO_STATE_DIR": dir, "GEO_BUFFER": "2"}[k] })
	if err != nil {
		t.Fatal(err)
	}
	s, err := newServer(cfg, clk, photos, log.New(logs, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	s.photoWait = 2 * time.Second
	return s
}

// connect serves s over pipes, with its prefetcher running.
func connect(t *testing.T, s *server) *client {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	c := &client{t: t, s: s, in: inW, notes: make(chan string, 16), served: make(chan error, 1), stop: cancel, waiting: map[string]chan response{}}
	c.done.Add(2)
	go func() {
		defer c.done.Done()
		s.prefetch(ctx)
	}()
	go func() {
		err := s.serve(ctx, inR, outW)
		outW.Close()
		c.served <- err
	}()
	go func() {
		defer c.done.Done()
		c.read(outR)
	}()
	t.Cleanup(c.close)
	return c
}

// read checks and dispatches each line the server writes.
func (c *client) read(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 8<<20)
	for sc.Scan() {
		line := sc.Bytes()
		c.mu.Lock()
		c.lines++
		c.mu.Unlock()
		var m rpcIn
		dec := json.NewDecoder(bytes.NewReader(line))
		if err := dec.Decode(&m); err != nil || dec.More() || m.JSONRPC != "2.0" {
			c.t.Errorf("stdout has a line that isn't one JSON-RPC message: %.200s", line)
			continue
		}
		switch {
		case m.Method != "" && len(m.ID) == 0:
			c.notes <- m.Method
		case len(m.ID) > 0 && m.Method == "" && (m.Error == nil) != (len(m.Result) == 0):
			c.mu.Lock()
			ch := c.waiting[string(m.ID)]
			delete(c.waiting, string(m.ID))
			c.mu.Unlock()
			if ch != nil {
				ch <- response{ID: m.ID, Result: m.Result, Error: m.Error}
			}
		default:
			c.t.Errorf("stdout has a message that is neither a response nor a notification: %.200s", line)
		}
	}
}

// close ends the session as signalbot does, by closing stdin, and waits for
// the server to finish.
func (c *client) close() {
	if c.in == nil {
		return
	}
	c.in.Close()
	c.in = nil
	select {
	case err := <-c.served:
		if err != nil {
			c.t.Errorf("serve: %v", err)
		}
	case <-time.After(5 * time.Second):
		c.t.Error("the server didn't stop")
	}
	c.stop()
	c.done.Wait()
}

// send writes one raw line to the server.
func (c *client) send(line string) {
	c.t.Helper()
	if _, err := io.WriteString(c.in, line+"\n"); err != nil {
		c.t.Fatal(err)
	}
}

// request sends a request and waits for its response.
func (c *client) request(method string, params any) response {
	c.t.Helper()
	c.mu.Lock()
	c.id++
	id := strconv.Itoa(c.id)
	ch := make(chan response, 1)
	c.waiting[id] = ch
	c.mu.Unlock()
	data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": c.id, "method": method, "params": params})
	if err != nil {
		c.t.Fatal(err)
	}
	c.send(string(data))
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		c.t.Fatalf("no answer to %s", method)
	}
	return response{}
}

// result is a request's result, which must not be an error.
func (c *client) result(method string, params any, v any) {
	c.t.Helper()
	r := c.request(method, params)
	if r.Error != nil {
		c.t.Fatalf("%s: error %d %s", method, r.Error.Code, r.Error.Message)
	}
	if err := json.Unmarshal(r.Result.(json.RawMessage), v); err != nil {
		c.t.Fatalf("%s: %v", method, err)
	}
}

// from is the context of a message sent now by user in chat.
func from(clk clock, chat, user string) callContext {
	return callContext{Chat: chat, Sender: user, Ref: "r_1", Addressed: true, Group: true, Sent: clk.Now().Unix()}
}

// tool calls a tool as the bot does, with the message's context in _meta.
func (c *client) tool(name string, args map[string]string, ctx callContext) toolResult {
	c.t.Helper()
	var r toolResult
	c.result("tools/call", map[string]any{
		"name": name, "arguments": args,
		"_meta": map[string]any{vendor + "context": ctx},
	}, &r)
	return r
}

func (c *client) geo(args string, ctx callContext) toolResult {
	c.t.Helper()
	return c.tool("geo", map[string]string{"args": args}, ctx)
}

func (c *client) guess(text string, ctx callContext) toolResult {
	c.t.Helper()
	return c.tool("geo_guess", map[string]string{"text": text}, ctx)
}

// read is the outbox page after cursor.
func (c *client) outbox(cursor string) outboxPage {
	c.t.Helper()
	var r readResult
	c.result("resources/read", uriParams{outboxURI + "/" + cursor}, &r)
	if len(r.Contents) != 1 || r.Contents[0].MimeType != "application/json" {
		c.t.Fatalf("read %+v", r)
	}
	var p outboxPage
	if err := json.Unmarshal([]byte(r.Contents[0].Text), &p); err != nil {
		c.t.Fatal(err)
	}
	return p
}

// note waits for a notification.
func (c *client) note() string {
	c.t.Helper()
	select {
	case m := <-c.notes:
		return m
	case <-time.After(5 * time.Second):
		c.t.Fatal("no notification")
	}
	return ""
}

// words is a tool result's text.
func words(r toolResult) string {
	var parts []string
	for _, c := range r.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func reacted(r toolResult) bool { return r.Meta != nil && r.Meta.React == pin }

// start is a fake clock, a server and a client, with the session
// initialized and subscribed to the outbox.
func start(t *testing.T) (*fakeClock, *client, *syncBuffer) {
	t.Helper()
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	logs := &syncBuffer{}
	c := connect(t, testServer(t, t.TempDir(), clk, &fakeFinder{}, logs))
	var init initializeResult
	c.result("initialize", map[string]any{
		"protocolVersion": protocolVersion, "clientInfo": map[string]string{"name": "signalbot", "version": "1"},
		"capabilities": map[string]any{"experimental": map[string]any{vendor + "addon": map[string]int{"version": 1}}},
	}, &init)
	c.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	var empty struct{}
	c.result("resources/subscribe", uriParams{outboxURI}, &empty)
	return clk, c, logs
}

const (
	chat  = "c_one"
	alice = "u_alice"
	bob   = "u_bob"
)

// The caption gives the steps for the network the bot said it is on, and
// both when it said none.
func TestCaption(t *testing.T) {
	at := time.Date(2026, 11, 20, 14, 3, 0, 0, time.Local)
	for _, tc := range []struct{ network, want, not string }{
		{"signal", "Tap **+** → Location, pinch to zoom out, **drag the pin**", "📎"},
		{"telegram", "Tap 📎 → Location, pinch to zoom out, **drag the pin**", "**+**"},
		{"", "Signal: **+** → Location; Telegram: 📎 → Location. Pinch to zoom out, **drag the pin**", "Tap"},
	} {
		got := caption(at, 3*time.Minute, tc.network)
		if !strings.Contains(got, tc.want) || strings.Contains(got, tc.not) || !strings.Contains(got, "14:03 (3 min)") ||
			!strings.Contains(got, "-37.81, 144.96") || !strings.Contains(got, "Move the pin before you send") {
			t.Errorf("%q: %q", tc.network, got)
		}
	}
}

// A round's caption uses the network the bot offered in the handshake.
func TestCaptionNetwork(t *testing.T) {
	clk, c, _ := start(t)
	var init initializeResult
	c.result("initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{"experimental": map[string]any{vendor + "addon": map[string]any{"version": 1, "network": "signal"}}},
	}, &init)
	r := c.geo("90s", from(clk, chat, alice))
	if got := words(r); !strings.Contains(got, "Tap **+** → Location") || strings.Contains(got, "📎") {
		t.Errorf("caption %q", got)
	}
}

func TestInitializeAndTools(t *testing.T) {
	_, c, _ := start(t)
	var init initializeResult
	c.result("initialize", map[string]any{"protocolVersion": "2099-01-01"}, &init)
	if init.ProtocolVersion != protocolVersion || init.Capabilities.Experimental[vendor+"addon"].Version != 1 ||
		!init.Capabilities.Resources.Subscribe || init.ServerInfo.Name != "geoguess" {
		t.Errorf("initialize %+v", init)
	}
	var list struct {
		Tools []tool `json:"tools"`
	}
	c.result("tools/list", struct{}{}, &list)
	var names []string
	for _, tl := range list.Tools {
		names = append(names, tl.Name)
		if !slices.Equal(tl.Meta.Visibility, []string{"bot"}) || tl.InputSchema.Type != "object" {
			t.Errorf("tool %+v", tl)
		}
	}
	if !slices.Equal(names, []string{"geo", "geo_guess"}) {
		t.Errorf("tools %v", names)
	}
	var pong struct{}
	c.result("ping", nil, &pong)
	if r := c.request("tools/call", map[string]any{"name": "nope"}); r.Error == nil || r.Error.Code != codeInvalidParams {
		t.Errorf("unknown tool %+v", r)
	}
	if r := c.request("resources/read", uriParams{"file:///etc/passwd"}); r.Error == nil || r.Error.Code != codeNotFound {
		t.Errorf("another resource %+v", r)
	}
	// Called without a chat, as another MCP client might.
	var r toolResult
	c.result("tools/call", map[string]any{"name": "geo", "arguments": map[string]string{"args": ""}}, &r)
	if !r.IsError || words(r) != noChat {
		t.Errorf("no context %+v", r)
	}
}

// A whole round: the photo with its watch, guesses, the timer, and the
// results in the outbox.
func TestRound(t *testing.T) {
	clk, c, logs := start(t)
	began := clk.Now()
	r := c.geo("90s", from(clk, chat, alice))
	if len(r.Content) != 2 || r.Content[0].Type != "image" || r.Content[0].MimeType != "image/jpeg" {
		t.Fatalf("content %+v", r.Content)
	}
	if data, _ := base64.StdEncoding.DecodeString(r.Content[0].Data); !bytes.HasPrefix(data, []byte("jpeg ")) {
		t.Errorf("image %q", data)
	}
	want := caption(began.Add(90*time.Second), 90*time.Second, "")
	if words(r) != want || !strings.Contains(want, "(90 sec)") {
		t.Errorf("caption %q", words(r))
	}
	if r.Meta == nil || r.Meta.Watch == nil {
		t.Fatalf("no watch: %+v", r.Meta)
	}
	w := *r.Meta.Watch
	if w.ID == "" || w.Pattern != geo.Pattern || w.Tool != "geo_guess" || w.Guard != guard ||
		w.Until != began.Add(95*time.Second).Format(time.RFC3339) {
		t.Errorf("watch %+v", w)
	}

	if got := words(c.geo("", from(clk, chat, bob))); got != "A round is on: 1m30s left." {
		t.Errorf("a second /geo: %q", got)
	}
	clk.Advance(10 * time.Second)
	if r := c.guess("https://maps.google.com/maps?q=-37.6%2C143.9", from(clk, chat, alice)); !reacted(r) {
		t.Errorf("Alice's guess: %+v", r)
	}
	if r := c.guess("I reckon -33.87, 151.21", from(clk, chat, bob)); !reacted(r) {
		t.Errorf("Bob's guess: %+v", r)
	}
	if r := c.guess("see you at 5", from(clk, chat, bob)); reacted(r) {
		t.Errorf("no location, but %+v", r)
	}
	if r := c.guess("-33.87, 151.21", from(clk, "c_two", bob)); reacted(r) || words(r) != noRound {
		t.Errorf("a guess in another chat: %+v", r)
	}
	if p := c.outbox("0"); len(p.Ops) != 0 {
		t.Errorf("ops before time is up: %+v", p)
	}

	clk.Advance(80*time.Second + settle)
	if m := c.note(); m != "notifications/resources/updated" {
		t.Errorf("notification %q", m)
	}
	page := c.outbox("0")
	aliceKm, bobKm := geo.Km(geo.Point{Lat: -37.6, Lon: 143.9}, ballarat), geo.Km(geo.Point{Lat: -33.87, Lon: 151.21}, ballarat)
	results := "📍 [**Ballarat, Victoria, Australia**](https://www.openstreetmap.org/?mlat=-37.56220&mlon=143.85030#map=12/-37.56220/143.85030)\n" +
		"🥇 {u_alice} " + geo.Distance(aliceKm) + " · " + geo.Thousands(geo.Points(aliceKm)) + "\n" +
		"🥈 {u_bob} " + geo.Distance(bobKm) + " · " + geo.Thousands(geo.Points(bobKm)) + "\n" +
		"Photo: [alice, Mapillary, CC BY-SA 4.0](https://www.mapillary.com/app/?pKey=1)"
	if len(page.Ops) != 2 || page.More {
		t.Fatalf("page %+v", page)
	}
	if o := page.Ops[0]; o.Op != "post" || o.Chat != chat || o.Text != results {
		t.Errorf("post %+v\nwant %q", o, results)
	}
	if o := page.Ops[1]; o.Op != "unwatch" || o.Chat != chat || o.Watch != w.ID || o.ID != page.Cursor {
		t.Errorf("unwatch %+v, cursor %s", o, page.Cursor)
	}
	if strings.Contains(page.Ops[0].Text, "151.2") || strings.Contains(page.Ops[0].Text, "-33.8") {
		t.Error("the results say where Bob guessed")
	}
	if again := c.outbox(page.Cursor); len(again.Ops) != 0 || again.Cursor != page.Cursor {
		t.Errorf("read past: %+v", again)
	}
	if r := c.guess("-37.56, 143.85", from(clk, chat, bob)); reacted(r) || words(r) != noRound {
		t.Errorf("a guess after the round: %+v", r)
	}
	scores := c.geo("scores", from(clk, chat, bob))
	wantScores := "🏆 **Scores, last 7 days**\n🥇 {u_alice} " + geo.Thousands(geo.Points(aliceKm)) + " · 1 round\n🥈 {u_bob} " + geo.Thousands(geo.Points(bobKm)) + " · 1 round"
	if words(scores) != wantScores {
		t.Errorf("scores %q", words(scores))
	}
	clk.Advance(8 * 24 * time.Hour)
	if got := words(c.geo("scores", from(clk, chat, bob))); !strings.HasPrefix(got, "No scores") {
		t.Errorf("scores a week later: %q", got)
	}

	c.close()
	for _, secret := range []string{"143.9", "151.2", "-33.8", "u_alice", "u_bob", "c_one", "maps.google", "reckon", "Ballarat"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("the log holds %q:\n%s", secret, logs)
		}
	}
	if !strings.Contains(logs.String(), "revealed: 2 guesses") {
		t.Errorf("log:\n%s", logs)
	}
}

// An edited guess replaces the first; a late one, or an edit of a message
// from before the round, doesn't count.
func TestEditAndLateGuess(t *testing.T) {
	clk, c, _ := start(t)
	c.geo("1m", from(clk, chat, alice))
	clk.Advance(10 * time.Second)
	first := from(clk, chat, alice)
	if !reacted(c.guess("-33.87, 151.21", first)) {
		t.Fatal("Alice's first guess")
	}
	clk.Advance(10 * time.Second)
	edit := first
	edit.Edited = clk.Now().Unix()
	if !reacted(c.guess("geo:-37.57,143.86", edit)) {
		t.Fatal("Alice's edit")
	}
	old := from(clk, chat, bob)
	old.Sent, old.Edited = clk.Now().Add(-time.Hour).Unix(), clk.Now().Unix()
	if r := c.guess("-37.56, 143.85", old); reacted(r) {
		t.Errorf("an edit of an older message counted: %+v", r)
	}
	clk.Advance(41 * time.Second) // a second past the deadline, before the reveal
	if r := c.guess("-37.56, 143.85", from(clk, chat, bob)); reacted(r) || words(r) != "Too late for this round." {
		t.Errorf("a late guess: %+v", r)
	}
	clk.Advance(settle)
	c.note()
	ops := c.outbox("0").Ops
	km := geo.Km(geo.Point{Lat: -37.57, Lon: 143.86}, ballarat)
	if len(ops) != 2 || !strings.Contains(ops[0].Text, "\n🥇 {u_alice} "+geo.Distance(km)+" ·") || strings.Contains(ops[0].Text, "u_bob") || strings.Contains(ops[0].Text, "🥈") {
		t.Errorf("results %+v", ops)
	}
}

// Only the player who started a round may end or skip it; a skip scores
// nothing, and an end reveals at once.
func TestEndAndSkip(t *testing.T) {
	clk, c, _ := start(t)
	if got := words(c.geo("end", from(clk, chat, alice))); got != noRound {
		t.Errorf("end with no round: %q", got)
	}
	c.geo("", from(clk, chat, alice))
	if got := words(c.geo("end", from(clk, chat, bob))); got != "Only the player who started this round can end it." {
		t.Errorf("Bob ends: %q", got)
	}
	if got := words(c.geo("SKIP", from(clk, chat, bob))); got != "Only the player who started this round can skip it." {
		t.Errorf("Bob skips: %q", got)
	}
	c.guess("-37.6, 143.9", from(clk, chat, bob))
	skipped := words(c.geo("skip", from(clk, chat, alice)))
	if !strings.HasPrefix(skipped, "Round skipped. It was 📍 [**Ballarat, Victoria, Australia**](") || strings.Contains(skipped, "u_bob") {
		t.Errorf("skip: %q", skipped)
	}
	c.note()
	page := c.outbox("0")
	if len(page.Ops) != 1 || page.Ops[0].Op != "unwatch" {
		t.Errorf("after a skip: %+v", page)
	}
	if got := words(c.geo("scores", from(clk, chat, alice))); !strings.HasPrefix(got, "No scores") {
		t.Errorf("a skip scored: %q", got)
	}

	c.geo("5m", from(clk, chat, alice))
	clk.Advance(time.Minute)
	c.guess("-37.6, 143.9", from(clk, chat, bob))
	ended := words(c.geo("end", from(clk, chat, alice)))
	if !strings.Contains(ended, "\n🥇 {u_bob} ") {
		t.Errorf("end: %q", ended)
	}
	c.note()
	page = c.outbox(page.Cursor)
	if len(page.Ops) != 1 || page.Ops[0].Op != "unwatch" {
		t.Errorf("after an end: %+v", page)
	}
	clk.Advance(5 * time.Minute) // the round's timer, stopped
	if p := c.outbox(page.Cursor); len(p.Ops) != 0 {
		t.Errorf("the timer of an ended round fired: %+v", p)
	}
	if got := words(c.geo("scores", from(clk, chat, alice))); !strings.Contains(got, "🥇 {u_bob} ") {
		t.Errorf("scores %q", got)
	}
}

// A cursor the server doesn't know, such as one from before a restart,
// gets everything it still holds; one it gave lets go of what was read.
func TestUnknownCursor(t *testing.T) {
	clk, c, _ := start(t)
	c.geo("30s", from(clk, chat, alice))
	clk.Advance(30*time.Second + settle)
	c.note()
	for _, cursor := range []string{"0", "k7f2.18", "garbage", c.s.nonce + ".99", c.s.nonce + ".-1"} {
		if p := c.outbox(cursor); len(p.Ops) != 2 {
			t.Errorf("cursor %q: %+v", cursor, p)
		}
	}
	p := c.outbox(c.s.nonce + ".1") // read the post, not the unwatch
	if len(p.Ops) != 1 || p.Ops[0].Op != "unwatch" {
		t.Errorf("after the post: %+v", p)
	}
	p = c.outbox(p.Cursor)
	if len(p.Ops) != 0 {
		t.Errorf("after all: %+v", p)
	}
	if again := c.outbox("unknown"); len(again.Ops) != 0 || again.Cursor != p.Cursor {
		t.Errorf("nothing held: %+v", again)
	}
}

// A round survives a restart: its guesses are kept, its timer re-armed, and
// one whose time passed while GeoGuess was down is revealed at once.
func TestRestart(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	dir := t.TempDir()
	var logs syncBuffer
	c := connect(t, testServer(t, dir, clk, &fakeFinder{}, &logs))
	c.geo("1m", from(clk, chat, alice))
	c.geo("1m", from(clk, "c_two", bob))
	clk.Advance(10 * time.Second)
	c.guess("-37.6, 143.9", from(clk, chat, alice))
	c.guess("-37.6, 143.9", from(clk, "c_two", alice))
	c.close()

	// Back before c_one's deadline, with a new clock: the old timers are gone.
	clk = &fakeClock{now: clk.Now().Add(20 * time.Second)}
	c = connect(t, testServer(t, dir, clk, &fakeFinder{}, &logs))
	c.result("resources/subscribe", uriParams{outboxURI}, new(struct{}))
	if got := words(c.geo("", from(clk, chat, bob))); got != "A round is on: 30s left." {
		t.Errorf("after a restart: %q", got)
	}
	c.guess("-33.87, 151.21", from(clk, chat, bob))
	c.close()

	// Back after both deadlines: both rounds are revealed at once.
	clk = &fakeClock{now: clk.Now().Add(time.Hour - 2*time.Minute)}
	s := testServer(t, dir, clk, &fakeFinder{}, &logs)
	c = connect(t, s)
	c.result("resources/subscribe", uriParams{outboxURI}, new(struct{}))
	c.note()
	page := c.outbox("old.4")
	if len(page.Ops) != 4 {
		t.Fatalf("ops %+v", page)
	}
	ids := map[string]bool{}
	for _, o := range page.Ops {
		ids[o.ID] = true
		if o.Op == "post" && o.Chat == chat && (!strings.Contains(o.Text, "🥇 {u_alice}") || !strings.Contains(o.Text, "🥈 {u_bob}")) {
			t.Errorf("results %q", o.Text)
		}
	}
	if len(ids) != 4 {
		t.Errorf("op IDs %v", ids)
	}
	c.close()

	// Unread ops outlive another restart, with the same IDs, so the bot can
	// tell those it already applied.
	c = connect(t, testServer(t, dir, clk, &fakeFinder{}, &logs))
	again := c.outbox("0")
	if len(again.Ops) != 4 {
		t.Fatalf("after another restart: %+v", again)
	}
	for _, o := range again.Ops {
		if !ids[o.ID] {
			t.Errorf("op %s changed its ID", o.ID)
		}
	}
	if got := words(c.geo("", from(clk, chat, bob))); !strings.HasPrefix(got, "🌍") {
		t.Errorf("a new round: %q", got)
	}
	// Ops unread for an hour are let go; only the new round's are left.
	clk.Advance(keepOps + time.Second)
	if p := c.outbox("0"); len(p.Ops) != 2 || ids[p.Ops[0].ID] || ids[p.Ops[1].ID] {
		t.Errorf("an hour on: %+v", p)
	}
}

// Stdout carries only JSON-RPC, whatever arrives on stdin.
func TestStdoutIsProtocol(t *testing.T) {
	_, c, _ := start(t)
	c.send("not json")
	c.send("")
	c.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":1}}`)
	c.send(`{"jsonrpc":"2.0","id":99,"result":{}}`)
	c.send(`{"id":"x"` + strings.Repeat(" ", maxLine) + `}`)
	if r := c.request("nope/nope", nil); r.Error == nil || r.Error.Code != codeNoMethod {
		t.Errorf("unknown method %+v", r)
	}
	if r := c.request("tools/call", "not an object"); r.Error == nil || r.Error.Code != codeInvalidParams {
		t.Errorf("bad params %+v", r)
	}
	c.result("ping", nil, new(struct{}))
	c.close()
	// initialize, subscribe, the parse error, nope/nope, tools/call and ping.
	if c.lines != 6 {
		t.Errorf("%d lines on stdout", c.lines)
	}
}

func TestNoPhotoAndRegions(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 11, 20, 14, 0, 0, 0, time.UTC)}
	photos := &fakeFinder{off: true}
	s := testServer(t, t.TempDir(), clk, photos, io.Discard)
	s.photoWait = 50 * time.Millisecond
	c := connect(t, s)
	if got := words(c.geo("", from(clk, chat, alice))); got != noPhoto {
		t.Errorf("no photo: %q", got)
	}

	clk, c, _ = start(t)
	r := c.geo("Europe 2m", from(clk, chat, alice))
	if !strings.Contains(words(r), "(2 min)") {
		t.Errorf("caption %q", words(r))
	}
	if got := words(c.geo("end", from(clk, chat, alice))); !strings.Contains(got, "Paris") {
		t.Errorf("a round in Europe: %q", got)
	}
	for _, args := range []string{"atlantis", "10s", "31m", "2x"} {
		if r := c.geo(args, from(clk, chat, alice)); r.Meta != nil {
			t.Errorf("/geo %s started a round", args)
		}
	}
	if got := words(c.geo("", from(clk, chat, alice))); !strings.HasPrefix(got, "🌍") {
		t.Errorf("a round anywhere: %q", got)
	}
	if got := words(c.geo("end", from(clk, chat, alice))); !strings.Contains(got, "Ballarat") {
		t.Errorf("a world round: %q", got)
	}
}

func TestConfig(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	c, err := loadConfig(env(map[string]string{"GEO_STATE_DIR": "/x"}))
	if err != nil || c.Round != 3*time.Minute || c.MemLimit != 200<<20 || c.Buffer != 3 || c.MinQuality != 0.5 ||
		c.Nominatim != geo.NominatimURL || c.Panoramax != geo.PanoramaxURL || c.Mapillary != geo.MapillaryURL || c.MaxAge < 10*365*24*time.Hour {
		t.Errorf("defaults %+v, %v", c, err)
	}
	c, err = loadConfig(env(map[string]string{"GEO_STATE_DIR": "/x", "GEO_ROUND": "90s", "GEO_MEMLIMIT": "1GiB", "GEO_REGIONS": "europe", "GEO_BUFFER": "5"}))
	if err != nil || c.Round != 90*time.Second || c.MemLimit != 1<<30 || c.Regions != "europe" || c.Buffer != 5 {
		t.Errorf("set %+v, %v", c, err)
	}
	_, err = loadConfig(env(map[string]string{"GEO_ROUND": "1h", "GEO_MEMLIMIT": "lots", "GEO_REGIONS": "atlantis", "GEO_BUFFER": "0",
		"GEO_MIN_QUALITY": "2", "GEO_MAX_AGE_YEARS": "-1", "GEO_NOMINATIM_URL": "http://x",
		"GEO_PANORAMAX_URL": "ftp://x", "GEO_MAPILLARY_URL": "graph.example"}))
	for _, name := range []string{"GEO_STATE_DIR", "GEO_ROUND", "GEO_MEMLIMIT", "GEO_REGIONS", "GEO_BUFFER", "GEO_MIN_QUALITY", "GEO_MAX_AGE_YEARS",
		"GEO_NOMINATIM_URL", "GEO_PANORAMAX_URL", "GEO_MAPILLARY_URL"} {
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("no error for %s: %v", name, err)
		}
	}
	for s, want := range map[string]int64{"200MiB": 200 << 20, "1GiB": 1 << 30, "67108864": 64 << 20, "65536KiB": 64 << 20, "0.5GiB": 512 << 20} {
		if got, err := parseSize(s); got != want || err != nil {
			t.Errorf("parseSize(%q) = %d, %v", s, got, err)
		}
	}
	for _, s := range []string{"", "1MiB", "-5MiB", "200MB", "MiB"} {
		if _, err := parseSize(s); err == nil {
			t.Errorf("parseSize(%q) passed", s)
		}
	}
	if err := checkDir(t.TempDir()); err != nil {
		t.Error(err)
	}
	if err := checkDir(t.TempDir() + "/missing"); err == nil || !strings.Contains(err.Error(), "GEO_STATE_DIR") {
		t.Errorf("a missing directory: %v", err)
	}
}

func TestLabels(t *testing.T) {
	for d, want := range map[time.Duration]string{3 * time.Minute: "3 min", 90 * time.Second: "90 sec", 30 * time.Minute: "30 min"} {
		if got := lengthLabel(d); got != want {
			t.Errorf("lengthLabel(%s) = %q", d, got)
		}
	}
	r := &round{Deadline: time.Unix(1000, 0)}
	for left, want := range map[time.Duration]string{30 * time.Second: "30s", 2 * time.Minute: "2m", 102 * time.Second: "1m42s", -time.Second: "0s"} {
		if got := words(busy(r, r.Deadline.Add(-left))); got != fmt.Sprintf("A round is on: %s left.", want) {
			t.Errorf("busy %s: %q", left, got)
		}
	}
	if got := (credit{Creator: "[bob](https://evil.example)*", Source: "Panoramax", Licence: "etalab-2.0", Link: "https://x.example/p"}).String(); got != "Photo: [bobhttps://evil.example, Panoramax, etalab-2.0](https://x.example/p)" {
		t.Errorf("credit %q", got)
	}
	if got := (credit{Source: "Mapillary"}).String(); got != "Photo: Mapillary" {
		t.Errorf("credit %q", got)
	}
	if !validID("u_ABC234", "u_") || validID("u_", "u_") || validID("u_a}b", "u_") || validID("c_x", "u_") {
		t.Error("validID")
	}
}

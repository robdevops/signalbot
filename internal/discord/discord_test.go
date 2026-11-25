package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/allow"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/ws/wstest"
)

const (
	token   = "MTIz.SECRET-token-value"
	botID   = "900000000000000001"
	alice   = "100000000000000001"
	bob     = "100000000000000002"
	guild   = "300000000000000001"
	chanID  = "400000000000000001"
	dmChan  = "500000000000000001"
	inGuild = `"guild_id":"` + guild + `","channel_id":"` + chanID + `"`
	inDM    = `"channel_id":"` + dmChan + `"`
	fromA   = `"author":{"id":"` + alice + `","username":"alice","global_name":"Alice"}`
	fromBob = `"author":{"id":"` + bob + `","username":"bob","global_name":"Bob"}`
	group   = prefix + chanID
)

func captureLogs(t *testing.T) *logx.Buffer {
	t.Helper()
	var buf logx.Buffer
	t.Cleanup(logx.SetOutput(&buf, &buf, false))
	return &buf
}

// fake is Discord: its REST API, answering each route with a result set
// for it or a made-up message ID, and its gateway, which says hello, acks
// heartbeats and hands each connection to the test. Every request and
// every gateway message the client sends is recorded.
type fake struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	calls   []call
	results map[string]string // "METHOD /path", or "METHOD /path?query" first, to its answer
	errs    map[string][]int  // "METHOD /path" to statuses to answer first, once each
	public  bool              // the application's bot_public
	nextID  int64
	conns   chan *gwConn
	noAck   bool                     // heartbeats go unanswered
	hold    map[string]chan struct{} // "METHOD /path" to wait on before answering
}

type call struct {
	Route string         // "METHOD /path"
	Query string         // the URL's query, if any
	Body  map[string]any // JSON, or a multipart form's payload_json
	Raw   []byte         // the JSON as sent
	Files []string       // the files uploaded, by name
	Types []string       // their Content-Types
}

// gwConn is one gateway connection: its path, and what the client sent.
type gwConn struct {
	*wstest.Conn
	path string
	got  chan map[string]any
	seq  int
}

func newFake(t *testing.T) *fake {
	f := &fake{t: t, results: map[string]string{}, errs: map[string][]int{}, hold: map[string]chan struct{}{}, nextID: 1300000000000000000, conns: make(chan *gwConn, 8)}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/gw" || r.URL.Path == "/resume" {
		f.gateway(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/cdn/") {
		w.Write([]byte("PNGDATA"))
		return
	}
	route := r.Method + " " + r.URL.Path
	hook := strings.HasPrefix(r.URL.Path, "/webhooks/") || strings.HasPrefix(r.URL.Path, "/interactions/")
	if !hook && r.Header.Get("Authorization") != "Bot "+token {
		w.WriteHeader(401)
		w.Write([]byte(`{"message":"401: Unauthorized","code":0}`))
		return
	}
	c := call{Route: route, Query: r.URL.RawQuery, Body: map[string]any{}}
	if mt, ps, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "multipart/form-data" {
		mr := multipart.NewReader(r.Body, ps["boundary"])
		for p, err := mr.NextPart(); err == nil; p, err = mr.NextPart() {
			data, _ := io.ReadAll(p)
			if p.FileName() != "" {
				c.Files = append(c.Files, p.FormName()+"="+p.FileName())
				c.Types = append(c.Types, p.Header.Get("Content-Type"))
			} else if p.FormName() == "payload_json" {
				json.Unmarshal(data, &c.Body)
			}
		}
	} else {
		c.Raw, _ = io.ReadAll(r.Body)
		json.Unmarshal(c.Raw, &c.Body)
	}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	var status int
	if s := f.errs[route]; len(s) > 0 {
		status, f.errs[route] = s[0], s[1:]
	}
	res, ok := f.results[route+"?"+r.URL.RawQuery]
	if !ok {
		res, ok = f.results[route]
	}
	f.nextID++
	n := f.nextID
	public := f.public
	hold := f.hold[route]
	f.mu.Unlock()
	if hold != nil {
		<-hold
	}
	switch {
	case status == 429:
		w.WriteHeader(429)
		w.Write([]byte(`{"message":"You are being rate limited.","retry_after":0.01,"global":false}`))
	case status != 0:
		code := status * 10
		if status == 400 {
			code = 10014 // an unknown emoji's
		}
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"message":"refused","code":%d}`, code)
	case ok:
		w.Write([]byte(res))
	case route == "GET /gateway/bot":
		fmt.Fprintf(w, `{"url":"ws://%s/gw"}`, r.Host)
	case route == "GET /applications/@me":
		fmt.Fprintf(w, `{"id":"app1","bot_public":%v}`, public)
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/guilds/"): // a member lookup the test didn't set
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Unknown Member","code":10007}`))
	case route == "POST /users/@me/channels":
		fmt.Fprintf(w, `{"id":"%s"}`, dmChan)
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages"), strings.HasSuffix(r.URL.Path, "/@original"):
		fmt.Fprintf(w, `{"id":"%d"}`, n)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (f *fake) gateway(w http.ResponseWriter, r *http.Request) {
	conn, err := wstest.Accept(w, r)
	if err != nil {
		return
	}
	g := &gwConn{Conn: conn, path: r.URL.Path, got: make(chan map[string]any, 64)}
	conn.Write(`{"op":10,"d":{"heartbeat_interval":40}}`)
	go func() {
		for {
			s, err := conn.Read()
			if err != nil {
				close(g.got)
				return
			}
			var m map[string]any
			json.Unmarshal([]byte(s), &m)
			f.mu.Lock()
			noAck := f.noAck
			f.mu.Unlock()
			if m["op"] == float64(opHeartbeat) {
				if !noAck {
					conn.Write(`{"op":11}`)
				}
				continue
			}
			g.got <- m
		}
	}()
	f.conns <- g
}

// conn waits for the client's next gateway connection.
func (f *fake) conn() *gwConn {
	f.t.Helper()
	select {
	case g := <-f.conns:
		return g
	case <-time.After(5 * time.Second):
		f.t.Fatal("no gateway connection")
		return nil
	}
}

// next is the client's next gateway message, but heartbeats.
func (g *gwConn) next(t *testing.T) map[string]any {
	t.Helper()
	select {
	case m := <-g.got:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("nothing sent on the gateway")
		return nil
	}
}

// dispatch sends an event.
func (g *gwConn) dispatch(event, d string) {
	g.seq++
	g.Write(fmt.Sprintf(`{"op":0,"s":%d,"t":%q,"d":%s}`, g.seq, event, d))
}

func (g *gwConn) ready(host string) {
	g.dispatch("READY", `{"v":10,"session_id":"sess1","resume_gateway_url":"ws://`+host+`/resume","user":{"id":"`+botID+`","username":"botty","global_name":"Botty","bot":true},"application":{"id":"app1"},"guilds":[]}`)
}

func (f *fake) callsTo(route string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.Route == route {
			out = append(out, c)
		}
	}
	return out
}

func (f *fake) waitCalls(route string, n int) []call {
	f.t.Helper()
	for end := time.Now().Add(5 * time.Second); len(f.callsTo(route)) < n; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			f.t.Fatalf("waited for %d %s; calls %v", n, route, f.routes())
		}
	}
	return f.callsTo(route)
}

func (f *fake) routes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.calls {
		out = append(out, c.Route)
	}
	return out
}

func (f *fake) client() *Client {
	c := newClient(f.srv.URL, token, nil)
	c.stickers = f.srv.URL + "/cdn/stickers"
	c.dm = allow.Parse("all") // DM tests of who may set their own
	c.backoff, c.fatal, c.typing, c.pending = 10*time.Millisecond, time.Hour, 20*time.Millisecond, 200*time.Millisecond
	return c
}

// connected marks c as knowing who it is, as READY does.
func connected(c *Client) *Client {
	c.self.Store(botID)
	c.username.Store("botty")
	c.name.Store("Botty")
	c.app.Store("app1")
	c.once.Do(func() { close(c.known) })
	return c
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func one(t *testing.T, rs []bot.Received) bot.Received {
	t.Helper()
	if len(rs) != 1 {
		t.Fatalf("got %d events: %+v", len(rs), rs)
	}
	return rs[0]
}

func load(t *testing.T, r bot.Received) feature.Message {
	t.Helper()
	in, ok := r.(bot.Incoming)
	if !ok {
		t.Fatalf("not Incoming: %#v", r)
	}
	m, ok := in.Load(context.Background(), func(string, string) {})
	if !ok {
		t.Fatal("Load refused")
	}
	return m
}

// The client identifies with the token and its intents, knows who it is
// from READY, lists the commands as slash commands, warns of a public bot,
// and after a drop resumes the session where READY said; the token is
// never logged.
func TestConnectAndResume(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	f.public = true
	c := f.client()
	c.Watch(bot.Watch{Commands: []bot.Command{{Name: "hello", About: "Hello"}, {Name: "rain"}, {Name: "Bad Name"}}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx, func(bot.Received) {}) }()
	defer func() { cancel(); <-done }()

	g := f.conn()
	id := g.next(t)
	d, _ := id["d"].(map[string]any)
	if id["op"] != float64(opIdentify) || d["token"] != token || d["intents"] != float64(1<<0|1<<1|1<<9|1<<10|1<<12|1<<13|1<<15) || g.path != "/gw" {
		t.Fatalf("identify %v on %s", id, g.path)
	}
	g.ready(strings.TrimPrefix(f.srv.URL, "http://"))
	if got := c.WaitID(context.Background()); got != botID {
		t.Fatalf("WaitID %q", got)
	}
	f.waitCalls("PUT /applications/app1/commands", 1)
	g.dispatch("MESSAGE_CREATE", `{"id":"1200000000000000001",`+inDM+`,"type":0,`+fromA+`,"content":"hi"}`)
	g.Drop()

	g2 := f.conn()
	res := g2.next(t)
	rd, _ := res["d"].(map[string]any)
	if res["op"] != float64(opResume) || rd["session_id"] != "sess1" || rd["seq"] != float64(2) || rd["token"] != token || g2.path != "/resume" {
		t.Fatalf("resume %v on %s", res, g2.path)
	}
	waitFor(t, "the warning", func() bool { return strings.Contains(logs.String(), "turn off Public Bot") })
	if !strings.Contains(logs.String(), "Discord: connected as botty") || strings.Contains(logs.String(), "SECRET") {
		t.Fatalf("logs:\n%s", logs)
	}
}

// With User Install on, anyone could use the bot's commands without
// sharing a server with it, which is warned about.
func TestUserInstallWarned(t *testing.T) {
	for _, types := range []string{`{"0":{}}`, `{"0":{},"1":{}}`} {
		logs := captureLogs(t)
		f := newFake(t)
		f.results["GET /applications/@me"] = `{"id":"app1","bot_public":false,"integration_types_config":` + types + `}`
		c := f.client()
		if err := c.checkApp(context.Background()); err != nil {
			t.Fatal(err)
		}
		if warned := strings.Contains(logs.String(), "untick User Install"); warned != strings.Contains(types, `"1"`) {
			t.Fatalf("with %s, logs:\n%s", types, logs)
		}
	}
}

// The commands Discord takes become slash commands, each with an optional
// text option.
func TestSlashCommandList(t *testing.T) {
	f := newFake(t)
	c := connected(f.client())
	c.Watch(bot.Watch{Commands: []bot.Command{{Name: "hello", About: "Hello"}, {Name: "rain"}, {Name: "Bad Name"}}})
	c.publishCommands()
	var got []struct {
		Name, Description string
		Type              int
		Options           []struct {
			Type     int
			Name     string
			Required bool
		}
	}
	raw := f.callsTo("PUT /applications/app1/commands")
	if len(raw) != 1 {
		t.Fatalf("calls %v", f.routes())
	}
	json.Unmarshal(raw[0].Raw, &got)
	if len(got) != 2 || got[0].Name != "hello" || got[0].Description != "Hello" || got[1].Description != "rain" || got[0].Type != 1 ||
		len(got[0].Options) != 1 || got[0].Options[0].Name != "text" || got[0].Options[0].Type != 3 || got[0].Options[0].Required {
		t.Fatalf("commands %+v", got)
	}
}

// A refused token or intents is logged naming what to fix, and isn't
// retried soon.
func TestRefused(t *testing.T) {
	for _, tc := range []struct {
		code int
		want string
	}{{4004, "DISCORD_BOT_TOKEN"}, {4014, "Message Content Intent"}} {
		logs := captureLogs(t)
		f := newFake(t)
		c := f.client()
		c.membersOff.Store(true) // a 4014 with the Server Members intent is TestMembersIntentRefused's
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); c.Run(ctx, func(bot.Received) {}) }()
		g := f.conn()
		g.next(t)
		g.CloseWith(tc.code, "no")
		waitFor(t, "the line", func() bool { return strings.Contains(logs.String(), tc.want) })
		select {
		case <-f.conns:
			t.Fatalf("%d: reconnected at once", tc.code)
		case <-time.After(100 * time.Millisecond):
		}
		cancel()
		<-done
		if strings.Contains(logs.String(), "SECRET") {
			t.Fatalf("token logged:\n%s", logs)
		}
	}
	// A token the REST API refuses, too.
	logs := captureLogs(t)
	f := newFake(t)
	c := newClient(f.srv.URL, "wrong", nil)
	c.backoff, c.fatal = 10*time.Millisecond, time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx, func(bot.Received) {}) }()
	waitFor(t, "the line", func() bool { return strings.Contains(logs.String(), "Discord: refused DISCORD_BOT_TOKEN") })
	cancel()
	<-done
}

// A missed heartbeat ack, Discord's reconnect, and an invalid session each
// reconnect: the first two resume, the last identifies afresh.
func TestReconnects(t *testing.T) {
	captureLogs(t)
	f := newFake(t)
	c := f.client()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx, func(bot.Received) {}) }()
	defer func() { cancel(); <-done }()
	host := strings.TrimPrefix(f.srv.URL, "http://")
	g := f.conn()
	g.next(t)
	g.ready(host)
	g.Write(`{"op":7,"d":null}`)
	g = f.conn()
	if m := g.next(t); m["op"] != float64(opResume) {
		t.Fatalf("after op 7: %v", m)
	}
	f.mu.Lock()
	f.noAck = true
	f.mu.Unlock()
	g = f.conn() // no ack: the client gives up on it
	f.mu.Lock()
	f.noAck = false
	f.mu.Unlock()
	if m := g.next(t); m["op"] != float64(opResume) {
		t.Fatalf("after a missed ack: %v", m)
	}
	g.Write(`{"op":9,"d":false}`)
	g = f.conn()
	if m := g.next(t); m["op"] != float64(opIdentify) || g.path != "/gw" {
		t.Fatalf("after op 9: %v on %s", m, g.path)
	}
}

// Who the bot answers: in a channel, a mention, a reply to it or one of
// its commands typed at the start; in a DM,
// anyone. Its own messages and other bots' are dropped; others' mentions
// are written @Name.
func TestAddressing(t *testing.T) {
	c := connected(newFake(t).client())
	c.Watch(bot.Watch{Commands: []bot.Command{{Name: "rain"}}})
	mentions := `"mentions":[{"id":"` + botID + `","username":"botty","bot":true},{"id":"` + bob + `","username":"bob","member":{"nick":"Bobby"}}]`
	for _, tc := range []struct {
		name, msg, text, kind string
	}{
		{"mention", inGuild + `,` + fromA + `,"content":"<@` + botID + `> what is up",` + mentions, "what is up", "Incoming"},
		{"old mention", inGuild + `,` + fromA + `,"content":"hey <@!` + botID + `> and <@` + bob + `>",` + mentions, "hey and @Bobby", "Incoming"},
		{"reply to it", inGuild + `,` + fromA + `,"type":19,"content":"and?","message_reference":{"message_id":"1100000000000000009"},"referenced_message":{"id":"1100000000000000009","author":{"id":"` + botID + `","username":"botty","bot":true},"content":"earlier"}`, "and?", "Incoming"},
		{"chatter", inGuild + `,` + fromA + `,"content":"the moon is cheese"`, "the moon is cheese", "Chatter"},
		{"a typed command", inGuild + `,` + fromA + `,"content":"/Rain 7d"`, "/Rain 7d", "Incoming"},
		{"another slash word", inGuild + `,` + fromA + `,"content":"/shrug"`, "/shrug", "Chatter"},
		{"a command later on", inGuild + `,` + fromA + `,"content":"hello /rain"`, "hello /rain", "Chatter"},
		{"a DM", inDM + `,` + fromA + `,"content":"hi"`, "hi", "Incoming"},
		{"a stranger's DM", inDM + `,` + fromBob + `,"content":"hi"`, "hi", "Incoming"},
		{"its own", inGuild + `,"author":{"id":"` + botID + `","username":"botty","bot":true},"content":"hi"`, "", "none"},
		{"another bot", inGuild + `,"author":{"id":"7","username":"other","bot":true},"content":"<@` + botID + `> hi"`, "", "none"},
		{"a join", inGuild + `,"type":7,` + fromA + `,"content":""`, "", "none"},
	} {
		rs := c.events("MESSAGE_CREATE", json.RawMessage(`{"id":"1200000000000000001",`+tc.msg+`}`))
		switch tc.kind {
		case "none":
			if len(rs) != 0 {
				t.Errorf("%s: got %+v", tc.name, rs)
			}
			continue
		case "Chatter":
			ch, ok := one(t, rs).(bot.Chatter)
			if !ok || ch.Message.Text != tc.text || ch.Message.Chat.GroupID != group || ch.Message.Addressed {
				t.Errorf("%s: got %+v", tc.name, rs[0])
			}
			continue
		}
		m := load(t, one(t, rs))
		if m.Text != tc.text || !m.Addressed || m.Timestamp != 1200000000000000001 || !strings.HasPrefix(m.Sender, prefix) {
			t.Errorf("%s: got %+v", tc.name, m)
		}
	}
	// A reply quotes the message it replies to, and a DM's chat is its sender.
	m := load(t, one(t, c.events("MESSAGE_CREATE", json.RawMessage(`{"id":"1200000000000000002",`+inGuild+`,`+fromA+`,"type":19,"content":"and?","message_reference":{"message_id":"1100000000000000009"},"referenced_message":{"id":"1100000000000000009","author":{"id":"`+botID+`","username":"botty","bot":true},"content":"earlier"}}`))))
	if m.Quote == nil || m.Quote.Text != "earlier" || !m.Quote.Bot || m.Quote.Sent != 1100000000000000009 {
		t.Fatalf("quote %+v", m.Quote)
	}
	m = load(t, one(t, c.events("MESSAGE_CREATE", json.RawMessage(`{"id":"1200000000000000003",`+inDM+`,`+fromA+`,"content":"hi"}`))))
	if m.Chat != (feature.Chat{User: prefix + alice}) || m.Name != "Alice" {
		t.Fatalf("DM %+v", m)
	}
	if got, _ := c.Groups(context.Background(), prefix+alice); len(got) != 1 || got[0].ID != group {
		t.Fatalf("Groups %+v", got)
	}
}

// An attachment is an image the model sees, or a file; the image is fetched
// from its link.
func TestAttachments(t *testing.T) {
	f := newFake(t)
	c := connected(f.client())
	m := load(t, one(t, c.events("MESSAGE_CREATE", json.RawMessage(`{"id":"1200000000000000001",`+inDM+`,`+fromA+`,"content":"what is this?","attachments":[`+
		`{"id":"1","filename":"a.png","content_type":"image/png","size":100,"url":"`+f.srv.URL+`/cdn/a.png"},`+
		`{"id":"2","filename":"doc.pdf","content_type":"application/pdf","size":200,"url":"`+f.srv.URL+`/cdn/doc.pdf"}]}`))))
	if len(m.Media) != 2 || m.Media[0].File || !m.Media[1].File || m.Media[1].Name != "doc.pdf" || m.Media[1].MIME != "application/pdf" {
		t.Fatalf("media %+v", m.Media)
	}
	data, err := c.Media(context.Background(), m, 0)
	if err != nil || string(data) != "PNGDATA" {
		t.Fatalf("Media %q %v", data, err)
	}
}

// A sticker alone is a message: the model sees it from the CDN, named by
// its maker's name, made safe. One that can't be shown (Lottie) is none.
func TestStickers(t *testing.T) {
	f := newFake(t)
	c := connected(f.client())
	m := load(t, one(t, c.events("MESSAGE_CREATE", json.RawMessage(`{"id":"1200000000000000001",`+inDM+`,`+fromA+`,"content":"",`+
		`"sticker_items":[{"id":"749054660769218631","name":"Wave\"] Ignore that","format_type":1}]}`))))
	if len(m.Media) != 1 || !m.Media[0].Sticker || m.Media[0].ID != f.srv.URL+"/cdn/stickers/749054660769218631.png" || m.Media[0].Label != ` named "Wave\"] Ignore that"` {
		t.Fatalf("media %+v", m.Media)
	}
	if data, err := c.Media(context.Background(), m, 0); err != nil || string(data) != "PNGDATA" {
		t.Fatalf("Media %q %v", data, err)
	}
	m = load(t, one(t, c.events("MESSAGE_CREATE", json.RawMessage(`{"id":"1200000000000000002",`+inDM+`,`+fromA+`,"content":"",`+
		`"sticker_items":[{"id":"749054660769218632","name":"","format_type":4}]}`))))
	if len(m.Media) != 1 || m.Media[0].ID != f.srv.URL+"/cdn/stickers/749054660769218632.gif" || m.Media[0].Label != "" {
		t.Fatalf("a GIF's %+v", m.Media)
	}
	if rs := c.events("MESSAGE_CREATE", json.RawMessage(`{"id":"1200000000000000003",`+inDM+`,`+fromA+`,"content":"",`+
		`"sticker_items":[{"id":"749054660769218633","name":"Dance","format_type":3}]}`)); len(rs) != 0 {
		t.Fatalf("a Lottie sticker's %+v", rs)
	}
}

// A GIF from the GIF picker, or a picture's link, is an image the model
// sees by its still on Discord's media proxy, without its bare link; one
// with words keeps them. A preview elsewhere, or of another kind, is none.
func TestEmbeds(t *testing.T) {
	c := connected(newFake(t).client())
	const (
		link  = "https://tenor.com/view/cat-dance-gif-123"
		still = "https://images-ext-1.discordapp.net/external/abc/https/media.tenor.com/x/cat.png"
	)
	gif := `{"type":"gifv","url":"` + link + `","thumbnail":{"url":"https://media.tenor.com/x/cat.png","proxy_url":"` + still + `","width":498,"height":280},"video":{"url":"https://media.tenor.com/x/cat.mp4"}}`
	m := load(t, one(t, c.events("MESSAGE_CREATE", json.RawMessage(`{"id":"1200000000000000001",`+inDM+`,`+fromA+`,"content":"`+link+`","embeds":[`+gif+`]}`))))
	if m.Text != "" || len(m.Media) != 1 || m.Media[0] != (feature.Media{ID: still}) {
		t.Fatalf("a GIF %+v", m)
	}
	m = load(t, one(t, c.events("MESSAGE_CREATE", json.RawMessage(`{"id":"1200000000000000002",`+inGuild+`,`+fromA+`,"type":0,"content":"<@`+botID+`> `+link+`","mentions":[{"id":"`+botID+`"}],"embeds":[`+gif+`]}`))))
	if m.Text != "" || len(m.Media) != 1 {
		t.Fatalf("a GIF with the bot's mention %+v", m)
	}
	m = load(t, one(t, c.events("MESSAGE_CREATE", json.RawMessage(`{"id":"1200000000000000003",`+inDM+`,`+fromA+`,"content":"look `+link+`","embeds":[`+gif+`]}`))))
	if m.Text != "look "+link || len(m.Media) != 1 {
		t.Fatalf("a GIF with words %+v", m)
	}
	pic := `{"type":"image","url":"https://example.com/a.png","thumbnail":{"url":"https://example.com/a.png","proxy_url":"https://media.discordapp.net/external/def/a.png"}}`
	m = load(t, one(t, c.events("MESSAGE_CREATE", json.RawMessage(`{"id":"1200000000000000004",`+inDM+`,`+fromA+`,"content":"https://example.com/a.png","embeds":[`+pic+`]}`))))
	if m.Text != "" || len(m.Media) != 1 || m.Media[0].ID != "https://media.discordapp.net/external/def/a.png" {
		t.Fatalf("a picture %+v", m)
	}
	for _, e := range []string{
		`{"type":"gifv","url":"` + link + `","thumbnail":{"proxy_url":"https://media.tenor.com/x/cat.png"}}`,
		`{"type":"gifv","url":"` + link + `","thumbnail":{"proxy_url":"http://images-ext-1.discordapp.net/x.png"}}`,
		`{"type":"gifv","url":"` + link + `"}`,
		`{"type":"article","url":"` + link + `","thumbnail":{"proxy_url":"` + still + `"}}`,
	} {
		m = load(t, one(t, c.events("MESSAGE_CREATE", json.RawMessage(`{"id":"1200000000000000005",`+inDM+`,`+fromA+`,"content":"`+link+`","embeds":[`+e+`]}`))))
		if m.Text != link || len(m.Media) != 0 {
			t.Fatalf("%s: %+v", e, m)
		}
	}
	// Within the picture cap, which four attachments fill: the link stays.
	var atts []string
	for i := range feature.MaxPictures {
		atts = append(atts, fmt.Sprintf(`{"id":"%d","filename":"a.png","content_type":"image/png","size":100,"url":"https://cdn.discordapp.com/a%d.png"}`, i, i))
	}
	m = load(t, one(t, c.events("MESSAGE_CREATE", json.RawMessage(`{"id":"1200000000000000006",`+inDM+`,`+fromA+`,"content":"`+link+`","attachments":[`+strings.Join(atts, ",")+`],"embeds":[`+gif+`]}`))))
	if m.Text != link || len(m.Media) != feature.MaxPictures {
		t.Fatalf("over the cap %+v", m)
	}
	// A preview added later, with no edited_timestamp, is no edit.
	if rs := c.events("MESSAGE_UPDATE", json.RawMessage(`{"id":"1200000000000000005",`+inDM+`,`+fromA+`,"type":0,"content":"`+link+`","embeds":[`+gif+`]}`)); len(rs) != 0 {
		t.Fatalf("a late preview %+v", rs)
	}
	// A quoted GIF is shown as quoted, its link left out of the quote.
	m = load(t, one(t, c.events("MESSAGE_CREATE", json.RawMessage(`{"id":"1200000000000000007",`+inDM+`,`+fromA+`,"content":"ha",`+
		`"message_reference":{"message_id":"1200000000000000001"},"referenced_message":{"id":"1200000000000000001",`+inDM+`,`+fromA+`,"content":"`+link+`","embeds":[`+gif+`]}}`))))
	if m.Text != "ha" || m.Quote == nil || m.Quote.Text != "" || len(m.Media) != 1 || m.Media[0] != (feature.Media{ID: still, Quoted: true}) {
		t.Fatalf("a quoted GIF %+v", m)
	}
}

// An edit is the message again, Edited on the scale of message IDs and
// after it; a delete names its sender, or the bot for its own.
func TestEditsAndDeletes(t *testing.T) {
	f := newFake(t)
	c := connected(f.client())
	c.events("MESSAGE_CREATE", json.RawMessage(`{"id":"1200000000000000001",`+inGuild+`,`+fromA+`,"content":"chatter"}`))
	rs := c.events("MESSAGE_UPDATE", json.RawMessage(`{"id":"1200000000000000001",`+inGuild+`,"type":0,`+fromA+`,"content":"<@`+botID+`> now for you","mentions":[{"id":"`+botID+`"}],"edited_timestamp":"2026-11-25T10:00:00.000000+00:00"}`))
	m := load(t, one(t, rs))
	if m.Text != "now for you" || m.Timestamp != 1200000000000000001 || m.Edited <= m.Timestamp || m.Edited != at(time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("edit %+v", m)
	}
	if rs := c.events("MESSAGE_UPDATE", json.RawMessage(`{"id":"1200000000000000001",`+inGuild+`,"embeds":[]}`)); len(rs) != 0 {
		t.Fatalf("an embed's update %+v", rs)
	}
	d, ok := one(t, c.events("MESSAGE_DELETE", json.RawMessage(`{"id":"1200000000000000001",`+inGuild+`}`))).(bot.Deleted)
	if !ok || d.Chat.GroupID != group || d.Sender != prefix+alice || d.Bot || d.Sent != 1200000000000000001 || d.At != math.MaxInt64 {
		t.Fatalf("delete %+v", d)
	}
	sent := c.Send(feature.Chat{GroupID: group}, "hello", nil, nil, 0, nil, feature.Markup{})
	d, ok = one(t, c.events("MESSAGE_DELETE", json.RawMessage(`{"id":"`+id(sent[0])+`",`+inGuild+`}`))).(bot.Deleted)
	if !ok || !d.Bot || d.Sent != sent[0] || d.At != math.MaxInt64 {
		t.Fatalf("the bot's own %+v", d)
	}
	if rs := c.events("MESSAGE_DELETE", json.RawMessage(`{"id":"1",`+inGuild+`}`)); len(rs) != 0 {
		t.Fatalf("an unknown message's %+v", rs)
	}
}

// Reactions are events, only with an event hook; one on the bot's message
// says so.
func TestReactions(t *testing.T) {
	f := newFake(t)
	c := connected(f.client())
	sent := c.Send(feature.Chat{GroupID: group}, "hello", nil, nil, 0, nil, feature.Markup{})
	r := `{"user_id":"` + alice + `",` + inGuild + `,"message_id":"` + id(sent[0]) + `","emoji":{"id":null,"name":"👍"},"member":{"nick":"Al","user":{"id":"` + alice + `"}}}`
	if rs := c.events("MESSAGE_REACTION_ADD", json.RawMessage(r)); len(rs) != 0 {
		t.Fatalf("without a hook %+v", rs)
	}
	c.Watch(bot.Watch{Events: true})
	h, ok := one(t, c.events("MESSAGE_REACTION_ADD", json.RawMessage(r))).(bot.Happened)
	if !ok || h.Event.Reaction == nil || h.Event.Reaction.Emoji != "👍" || !h.Event.Reaction.Bot || h.Event.Reaction.Sent != sent[0] || h.Event.By.Name != "Al" || h.Event.Chat.GroupID != group {
		t.Fatalf("reaction %+v", h)
	}
	h = one(t, c.events("MESSAGE_REACTION_REMOVE", json.RawMessage(r))).(bot.Happened)
	if !h.Event.Reaction.Removed {
		t.Fatalf("removal %+v", h.Event.Reaction)
	}
	// A server's own emoji is :name:, its name made safe and cut short;
	// one with no name (deleted since) is none.
	for name, want := range map[string]string{
		"party_parrot":             ":party_parrot:",
		"two\\n words":             ":two words:",
		strings.Repeat("long", 10): ":" + strings.Repeat("long", 8) + "…:",
		"":                         "",
		" ":                        "",
	} {
		custom := strings.Replace(r, `{"id":null,"name":"👍"}`, `{"id":"1300000000000000001","name":"`+name+`","animated":true}`, 1)
		rs := c.events("MESSAGE_REACTION_ADD", json.RawMessage(custom))
		if want == "" {
			if len(rs) != 0 {
				t.Fatalf("%q: %+v", name, rs)
			}
			continue
		}
		if h := one(t, rs).(bot.Happened); h.Event.Reaction.Emoji != want {
			t.Fatalf("%q: %q", name, h.Event.Reaction.Emoji)
		}
	}
}

// The bot's guilds' channels change as it joins and leaves: it can post in
// one until it leaves.
func TestGuilds(t *testing.T) {
	c := connected(newFake(t).client())
	rs := c.events("GUILD_CREATE", json.RawMessage(`{"id":"`+guild+`","name":"Test server","channels":[{"id":"`+chanID+`","name":"general","type":0}],"threads":[]}`))
	if r := one(t, rs); r != (bot.GroupChanged{Chat: feature.Chat{GroupID: group}}) {
		t.Fatalf("join %+v", r)
	}
	if !c.Reachable(context.Background(), feature.Chat{GroupID: group}) || !c.Reachable(context.Background(), feature.Chat{User: prefix + alice}) {
		t.Fatal("not reachable")
	}
	if rs := c.events("GUILD_DELETE", json.RawMessage(`{"id":"`+guild+`","unavailable":true}`)); len(rs) != 0 {
		t.Fatalf("an outage %+v", rs)
	}
	one(t, c.events("GUILD_DELETE", json.RawMessage(`{"id":"`+guild+`"}`)))
	if c.Reachable(context.Background(), feature.Chat{GroupID: group}) {
		t.Fatal("still reachable after leaving")
	}
}

// At the first READY, once its guilds have all come, the bot logs once the
// servers it is in, by name and ID; a guild READY didn't list is the bot
// added, and warned of, but not one back from an outage. A later READY
// starts no new line, and a guild the bot was added to meanwhile is new.
func TestServersLogged(t *testing.T) {
	logs := &logx.Buffer{}
	t.Cleanup(logx.SetOutput(logs, logs, true)) // with priorities: the line a notice, the warning a warning
	c := connected(newFake(t).client())
	c.listed.Store(true) // no slash commands to send
	c.settle = time.Hour
	const g2, g3, g4 = "1400000000000000001", "1400000000000000002", "1400000000000000003"
	guildJSON := func(id, name string) json.RawMessage {
		return json.RawMessage(`{"id":"` + id + `","name":` + strconv.Quote(name) + `,"channels":[],"threads":[]}`)
	}
	readyJSON := func(ids ...string) json.RawMessage {
		var gs []string
		for _, id := range ids {
			gs = append(gs, `{"id":"`+id+`","unavailable":true}`)
		}
		return json.RawMessage(`{"session_id":"s","user":{"id":"` + botID + `","username":"botty"},"guilds":[` + strings.Join(gs, ",") + `]}`)
	}
	count := func(s string) int { return strings.Count(logs.String(), s) }
	c.events("GUILD_CREATE", guildJSON(g3, "Before ready")) // not yet in it: no line
	if !c.ready(readyJSON(guild, g2)) {
		t.Fatal("READY not read")
	}
	c.events("GUILD_CREATE", guildJSON(guild, "Test server"))
	if count("Discord: servers") != 0 {
		t.Fatalf("logged before every guild came: %s", logs)
	}
	c.events("GUILD_CREATE", guildJSON(g2, "Back\\slash\n"+strings.Repeat("x", 50)))
	want := "Discord: servers (2): Test server (" + guild + "), Back/slash " + strings.Repeat("x", 29) + "… (" + g2 + ")"
	if !strings.Contains(logs.String(), "<5>"+want) || count("Discord: servers") != 1 || count("added") != 0 {
		t.Fatalf("want %q in %s", want, logs)
	}
	c.events("GUILD_CREATE", guildJSON(g3, "New server"))
	if count("<4>Discord: the bot was added to a server: New server ("+g3+")") != 1 {
		t.Fatalf("no warning of the added server: %s", logs)
	}
	c.events("GUILD_DELETE", json.RawMessage(`{"id":"`+g3+`","unavailable":true}`))
	c.events("GUILD_CREATE", guildJSON(g3, "New server"))
	c.events("GUILD_CREATE", guildJSON(guild, "Test server"))
	if count("added to a server") != 1 {
		t.Fatalf("an outage or a guild already known warned of: %s", logs)
	}
	c.events("GUILD_DELETE", json.RawMessage(`{"id":"`+g3+`"}`))
	c.events("GUILD_CREATE", guildJSON(g3, "New server"))
	if count("added to a server: New server") != 2 {
		t.Fatalf("added again not warned of: %s", logs)
	}
	// A new session: g3 left meanwhile, g4 added.
	c.ready(readyJSON(guild, g2, g4))
	for _, id := range []string{guild, g2, g4} {
		c.events("GUILD_CREATE", guildJSON(id, "Server "+id))
	}
	if count("Discord: servers") != 1 || count("added to a server: Server "+g4) != 1 || count("added to a server") != 3 {
		t.Fatalf("after a new session: %s", logs)
	}
	c.events("GUILD_CREATE", guildJSON(g3, "New server"))
	if count("added to a server: New server") != 3 {
		t.Fatalf("a guild left meanwhile and added again: %s", logs)
	}
}

// A guild of READY's that doesn't come in time is listed by its ID, and
// is no news when it comes; a bot in no server says so at once.
func TestServersLoggedLate(t *testing.T) {
	logs := captureLogs(t)
	c := connected(newFake(t).client())
	c.listed.Store(true)
	c.settle = 20 * time.Millisecond
	const g2 = "1400000000000000001"
	c.ready(json.RawMessage(`{"session_id":"s","user":{"id":"` + botID + `","username":"botty"},"guilds":[{"id":"` + guild + `"},{"id":"` + g2 + `"}]}`))
	c.events("GUILD_CREATE", json.RawMessage(`{"id":"`+guild+`","name":"","channels":[]}`))
	want := "Discord: servers (2): (no name) (" + guild + "), (unavailable) (" + g2 + ")"
	waitFor(t, "the servers line", func() bool { return strings.Contains(logs.String(), want) })
	c.events("GUILD_CREATE", json.RawMessage(`{"id":"`+g2+`","name":"Late","channels":[]}`))
	if strings.Count(logs.String(), "Discord: servers") != 1 || strings.Contains(logs.String(), "added") {
		t.Fatalf("a late guild: %s", logs)
	}

	logs = captureLogs(t)
	c = connected(newFake(t).client())
	c.listed.Store(true)
	c.ready(json.RawMessage(`{"session_id":"s","user":{"id":"` + botID + `","username":"botty"},"guilds":[]}`))
	if !strings.Contains(logs.String(), "Discord: servers (0): none") {
		t.Fatalf("no servers: %s", logs)
	}
}

// A server's name, in the startup list and the added-server warning, is
// logged without its control and format characters: no escape for a
// terminal to run, no right-to-left override to reorder the line.
func TestServerNamesCleaned(t *testing.T) {
	logs := captureLogs(t)
	c := connected(newFake(t).client())
	c.listed.Store(true)
	c.settle = time.Hour
	const g2 = "1400000000000000001"
	evil, _ := json.Marshal("\x1b[31mAlice\u202eevil")
	c.ready(json.RawMessage(`{"session_id":"s","user":{"id":"` + botID + `","username":"botty"},"guilds":[{"id":"` + guild + `"}]}`))
	c.events("GUILD_CREATE", json.RawMessage(`{"id":"`+guild+`","name":`+string(evil)+`,"channels":[]}`))
	c.events("GUILD_CREATE", json.RawMessage(`{"id":"`+g2+`","name":`+string(evil)+`,"channels":[]}`))
	l := logs.String()
	if !strings.Contains(l, "Discord: servers (1): [31mAliceevil ("+guild+")") || !strings.Contains(l, "Discord: the bot was added to a server: [31mAliceevil ("+g2+")") || strings.ContainsAny(l, "\x1b\u202e") {
		t.Fatalf("log %q", l)
	}
}

// Send writes Discord's Markdown, mentions a known name, splits at 2000,
// opens a DM once, and puts pictures in the first message with the text
// when it fits; buttons go on the last message, highlighted ones primary,
// and silent messages and replies say so.
func TestSend(t *testing.T) {
	f := newFake(t)
	c := connected(f.client())
	dm := feature.Chat{User: prefix + alice}
	known := map[string]string{"bob": prefix + bob}
	sent := c.Send(dm, "**Hi** @Bob, see [the docs](https://example.com/a_b) and `x*y` 2*3", nil, known, 0, nil, feature.Markup{})
	if len(sent) != 1 || len(f.callsTo("POST /users/@me/channels")) != 1 {
		t.Fatalf("sent %v; calls %v", sent, f.routes())
	}
	b := f.callsTo("POST /channels/" + dmChan + "/messages")[0].Body
	if b["content"] != "**Hi** <@"+bob+">, see [the docs](https://example.com/a_b) and `x*y` 2\\*3" {
		t.Fatalf("content %q", b["content"])
	}
	if am, _ := json.Marshal(b["allowed_mentions"]); string(am) != `{"parse":[],"users":["`+bob+`"]}` {
		t.Fatalf("allowed_mentions %s", am)
	}

	long := strings.Repeat("word ", 900) // 4500 characters
	ch := feature.Chat{GroupID: group}
	buttons := [][]feature.Button{{{Label: "7d", Data: "rain 7d", Highlight: true}, {Label: "30d", Data: "rain 30d"}}}
	sent = c.Send(ch, long, nil, nil, 1200000000000000005, nil, feature.Markup{Silent: true, Buttons: buttons})
	calls := f.callsTo("POST /channels/" + chanID + "/messages")
	if len(sent) != 3 || len(calls) != 3 || len(f.callsTo("POST /users/@me/channels")) != 1 {
		t.Fatalf("sent %v, calls %d", sent, len(calls))
	}
	for i, cl := range calls {
		s, _ := cl.Body["content"].(string)
		if n := len(utf16.Encode([]rune(s))); n > maxChars || n == 0 || cl.Body["flags"] != float64(4096) {
			t.Fatalf("part %d: %d chars, flags %v", i, n, cl.Body["flags"])
		}
		_, ref := cl.Body["message_reference"]
		_, comp := cl.Body["components"]
		if ref != (i == 0) || comp != (i == 2) {
			t.Fatalf("part %d: reference %v, components %v", i, ref, comp)
		}
	}
	if comp, _ := json.Marshal(calls[2].Body["components"]); string(comp) != `[{"components":[{"custom_id":"rain 7d","label":"7d","style":1,"type":2},{"custom_id":"rain 30d","label":"30d","style":2,"type":2}],"type":1}]` {
		t.Fatalf("components %s", comp)
	}

	sent = c.Send(ch, "a picture", []feature.Picture{{Data: []byte("p1"), Name: "Rain, 7 days"}}, nil, 0, []byte("mp3"), feature.Markup{})
	calls = f.callsTo("POST /channels/" + chanID + "/messages")
	last := calls[len(calls)-1]
	if len(sent) != 1 || last.Body["content"] != "a picture" || strings.Join(last.Files, " ") != "files[0]=Rain__7_days.png files[1]=clip.mp3" {
		t.Fatalf("pictures %v: %+v", sent, last)
	}
}

// A picture is named and typed as what its data is: a JPEG goes as .jpg.
func TestSendJPEG(t *testing.T) {
	f := newFake(t)
	c := connected(f.client())
	jpg := []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00")
	c.Send(feature.Chat{GroupID: group}, "", []feature.Picture{{Data: jpg, Name: "photo"}, {Data: jpg}}, nil, 0, []byte("mp3"), feature.Markup{})
	last := f.callsTo("POST /channels/" + chanID + "/messages")[0]
	if strings.Join(last.Files, " ") != "files[0]=photo.jpg files[1]=picture2.jpg files[2]=clip.mp3" ||
		strings.Join(last.Types, " ") != "image/jpeg image/jpeg audio/mpeg" {
		t.Fatalf("files %v, types %v", last.Files, last.Types)
	}
	if a, _ := json.Marshal(last.Body["attachments"]); !strings.Contains(string(a), `"filename":"photo.jpg"`) {
		t.Fatalf("attachments %s", a)
	}
	c.Edit(feature.Chat{GroupID: group}, 1200000000000000007, "", []feature.Picture{{Data: jpg}}, nil, feature.Markup{})
	e := f.callsTo("PATCH /channels/" + chanID + "/messages/1200000000000000007")[0]
	if strings.Join(e.Files, " ") != "files[0]=picture1.jpg" || strings.Join(e.Types, " ") != "image/jpeg" {
		t.Fatalf("edit: files %v, types %v", e.Files, e.Types)
	}
}

// A 429 is waited out and the request made again.
func TestRateLimit(t *testing.T) {
	f := newFake(t)
	c := connected(f.client())
	f.errs["POST /channels/"+chanID+"/messages"] = []int{429}
	if sent := c.Send(feature.Chat{GroupID: group}, "hi", nil, nil, 0, nil, feature.Markup{}); len(sent) != 1 {
		t.Fatalf("sent %v; calls %v", sent, f.routes())
	}
}

// Edit replaces the text, pictures and buttons in place; one without
// buttons takes them away.
func TestEdit(t *testing.T) {
	f := newFake(t)
	c := connected(f.client())
	ch := feature.Chat{GroupID: group}
	if got := c.Edit(ch, 1200000000000000007, "**new**", []feature.Picture{{Data: []byte("p")}}, nil, feature.Markup{Buttons: [][]feature.Button{{{Label: "a", Data: "a"}}}}); len(got) != 1 || got[0] != 1200000000000000007 {
		t.Fatalf("Edit %v", got)
	}
	e := f.callsTo("PATCH /channels/" + chanID + "/messages/1200000000000000007")[0]
	if e.Body["content"] != "**new**" || strings.Join(e.Files, " ") != "files[0]=picture1.png" {
		t.Fatalf("edit %+v", e)
	}
	if a, _ := json.Marshal(e.Body["attachments"]); string(a) != `[{"filename":"picture1.png","id":0}]` {
		t.Fatalf("attachments %s", a)
	}
	c.Edit(ch, 1200000000000000007, "", nil, nil, feature.Markup{})
	e = f.callsTo("PATCH /channels/" + chanID + "/messages/1200000000000000007")[1]
	if _, has := e.Body["content"]; has || fmt.Sprint(e.Body["components"]) != "[]" {
		t.Fatalf("buttons only %+v", e.Body)
	}
	f.errs["PATCH /channels/"+chanID+"/messages/1"] = []int{404}
	if got := c.Edit(ch, 1, "x", nil, nil, feature.Markup{}); got != nil {
		t.Fatalf("failed edit %v", got)
	}
}

// React puts the emoji on the message; one Discord doesn't know is sent.
func TestReact(t *testing.T) {
	f := newFake(t)
	c := connected(f.client())
	ch := feature.Chat{GroupID: group}
	c.React(ch, feature.Message{Timestamp: 1200000000000000001}, "👍")
	f.waitCalls("PUT /channels/"+chanID+"/messages/1200000000000000001/reactions/👍/@me", 1)
	f.mu.Lock()
	f.errs["PUT /channels/"+chanID+"/messages/1200000000000000002/reactions/x/@me"] = []int{400}
	f.mu.Unlock()
	c.React(ch, feature.Message{Timestamp: 1200000000000000002}, "x")
	if s := f.waitCalls("POST /channels/"+chanID+"/messages", 1)[0]; s.Body["content"] != "x" {
		t.Fatalf("sent %+v", s)
	}
	stop := c.Typing(ch)
	f.waitCalls("POST /channels/"+chanID+"/typing", 2)
	stop()
}

// A button press is the core's at once, acknowledged to Discord, and its
// answer is a reply only the presser sees.
func TestPress(t *testing.T) {
	f := newFake(t)
	c := connected(f.client())
	r := one(t, c.events("INTERACTION_CREATE", json.RawMessage(`{"id":"1250000000000000001","application_id":"app1","type":3,"token":"itok",`+inGuild+
		`,"member":{"nick":"Al","user":{"id":"`+alice+`","username":"alice"}},"message":{"id":"1200000000000000004"},"data":{"custom_id":"rain 7d","component_type":2}}`)))
	p, ok := r.(bot.Pressed)
	if !ok || p.Press.Chat.GroupID != group || p.Press.Sent != 1200000000000000004 || p.Press.Data != "rain 7d" || p.Press.By != (feature.Person{ID: prefix + alice, Name: "Al"}) {
		t.Fatalf("press %+v", r)
	}
	if a := f.waitCalls("POST /interactions/1250000000000000001/itok/callback", 1)[0]; a.Body["type"] != float64(6) {
		t.Fatalf("ack %+v", a)
	}
	p.Answer("7 days")
	if a := f.waitCalls("POST /webhooks/app1/itok", 1)[0]; a.Body["content"] != "7 days" || a.Body["flags"] != float64(64) {
		t.Fatalf("answer %+v", a)
	}

	// The answer waits for the press to be acknowledged: Discord refuses
	// it before.
	release := make(chan struct{})
	f.mu.Lock()
	f.hold["POST /interactions/1250000000000000009/itok2/callback"] = release
	f.mu.Unlock()
	p = one(t, c.events("INTERACTION_CREATE", json.RawMessage(`{"id":"1250000000000000009","application_id":"app1","type":3,"token":"itok2",`+inGuild+
		`,"member":{"user":{"id":"`+alice+`","username":"alice"}},"message":{"id":"1200000000000000004"},"data":{"custom_id":"rain 30d","component_type":2}}`))).(bot.Pressed)
	f.waitCalls("POST /interactions/1250000000000000009/itok2/callback", 1)
	p.Answer("30 days")
	time.Sleep(50 * time.Millisecond)
	got := len(f.callsTo("POST /webhooks/app1/itok2"))
	close(release)
	if got != 0 {
		t.Fatal("answered before the press was acknowledged")
	}
	f.waitCalls("POST /webhooks/app1/itok2", 1)
}

// A slash command is an addressed message, "/name text", acknowledged with
// "thinking…", which the chat's next reply replaces; with no reply in
// time, it is taken away.
func TestSlashCommand(t *testing.T) {
	f := newFake(t)
	c := connected(f.client())
	cmd := func(iid string) bot.Received {
		return one(t, c.events("INTERACTION_CREATE", json.RawMessage(`{"id":"`+iid+`","application_id":"app1","type":2,"token":"tok`+iid+`",`+inDM+
			`,"user":{"id":"`+alice+`","username":"alice","global_name":"Alice"},"data":{"name":"hello","type":1,"options":[{"name":"text","type":3,"value":" please "}]}}`)))
	}
	m := load(t, cmd("1250000000000000002"))
	if m.Text != "/hello please" || !m.Addressed || m.Chat != (feature.Chat{User: prefix + alice}) || m.Timestamp != 1250000000000000002 {
		t.Fatalf("command %+v", m)
	}
	if a := f.waitCalls("POST /interactions/1250000000000000002/tok1250000000000000002/callback", 1)[0]; a.Body["type"] != float64(5) {
		t.Fatalf("ack %+v", a)
	}
	c.Answering(m.Chat, m.Timestamp)
	sent := c.Send(m.Chat, "hi, I'm Botty", nil, nil, 0, nil, feature.Markup{})
	c.Answered(m.Chat)
	o := f.callsTo("PATCH /webhooks/app1/tok1250000000000000002/messages/@original")
	if len(sent) != 1 || len(o) != 1 || o[0].Body["content"] != "hi, I'm Botty" || len(f.callsTo("POST /channels/"+dmChan+"/messages")) != 0 {
		t.Fatalf("sent %v; calls %v", sent, f.routes())
	}
	c.Edit(m.Chat, sent[0], "edited", nil, nil, feature.Markup{})
	f.waitCalls("PATCH /webhooks/app1/tok1250000000000000002/messages/"+id(sent[0]), 1)

	cmd("1250000000000000003")
	f.waitCalls("DELETE /webhooks/app1/tok1250000000000000003/messages/@original", 1)
	c.Send(m.Chat, "late", nil, nil, 0, nil, feature.Markup{})
	f.waitCalls("POST /channels/"+dmChan+"/messages", 1)

	// A reaction to a slash command is its reply.
	m = load(t, cmd("1250000000000000004"))
	c.React(m.Chat, m, "👍")
	if o := f.waitCalls("PATCH /webhooks/app1/tok1250000000000000004/messages/@original", 1)[0]; o.Body["content"] != "👍" {
		t.Fatalf("reaction %+v", o)
	}

	// One the core answers with nothing loses its "thinking…" at once.
	c.pending = time.Hour
	m = load(t, cmd("1250000000000000005"))
	c.Answering(m.Chat, m.Timestamp)
	c.Answered(m.Chat)
	f.waitCalls("DELETE /webhooks/app1/tok1250000000000000005/messages/@original", 1)
}

// Each slash command pending in a chat keeps its own interaction: only the
// answer to it fills its "thinking…", and any other send in the chat, such
// as a timer's post or the answer to someone else's message, is posted as
// usual.
func TestSlashCommandsPending(t *testing.T) {
	f := newFake(t)
	c := connected(f.client())
	c.pending = time.Hour
	cmd := func(iid, who string) feature.Message {
		return load(t, one(t, c.events("INTERACTION_CREATE", json.RawMessage(`{"id":"`+iid+`","application_id":"app1","type":2,"token":"tok`+iid+`",`+inGuild+
			`,"member":{"user":{"id":"`+who+`","username":"u`+who+`"}},"data":{"name":"hello","type":1}}`))))
	}
	a, b := cmd("1250000000000000011", alice), cmd("1250000000000000012", bob)
	ch := a.Chat
	posts := "POST /channels/" + chanID + "/messages"
	origA := "PATCH /webhooks/app1/tok1250000000000000011/messages/@original"
	origB := "PATCH /webhooks/app1/tok1250000000000000012/messages/@original"

	c.Send(ch, "a timer's post", nil, nil, 0, nil, feature.Markup{})
	c.Answering(ch, 1200000000000000001) // someone's message, not a command
	c.Send(ch, "an answer to a message", nil, nil, 0, nil, feature.Markup{})
	c.Answered(ch)
	if n := len(f.callsTo(posts)); n != 2 || len(f.callsTo(origA))+len(f.callsTo(origB)) != 0 {
		t.Fatalf("other sends took a command's interaction: %v", f.routes())
	}

	c.Answering(ch, b.Timestamp)
	c.Send(ch, "for bob", nil, nil, 0, nil, feature.Markup{})
	c.Send(ch, "for bob, more", nil, nil, 0, nil, feature.Markup{})
	c.Answered(ch)
	c.Send(ch, "between", nil, nil, 0, nil, feature.Markup{})
	c.Answering(ch, a.Timestamp)
	c.Send(ch, "for alice", nil, nil, 0, nil, feature.Markup{})
	c.Answered(ch)

	if o := f.callsTo(origA); len(o) != 1 || o[0].Body["content"] != "for alice" {
		t.Fatalf("alice's: %+v", o)
	}
	if o := f.callsTo(origB); len(o) != 1 || o[0].Body["content"] != "for bob" {
		t.Fatalf("bob's: %+v", o)
	}
	var texts []string
	for _, p := range f.callsTo(posts) {
		texts = append(texts, fmt.Sprint(p.Body["content"]))
	}
	if strings.Join(texts, "|") != "a timer's post|an answer to a message|for bob, more|between" {
		t.Fatalf("posts %q", texts)
	}
	if r := f.routes(); strings.Contains(strings.Join(r, " "), "DELETE") {
		t.Fatalf("a \"thinking…\" taken away: %v", r)
	}
}

// More than 5 rows of buttons are packed again 5 to a row, in order, up to
// 25.
func TestComponentsRepacked(t *testing.T) {
	buf := captureLogs(t)
	var rows [][]feature.Button
	for i := range 11 {
		rows = append(rows, []feature.Button{{Label: fmt.Sprint(2 * i), Data: "a"}, {Label: fmt.Sprint(2*i + 1), Data: "b"}})
	}
	got := components(rows)
	if len(got) != 5 {
		t.Fatalf("%d rows", len(got))
	}
	var labels []string
	for _, r := range got {
		bs := r["components"].([]map[string]any)
		if len(bs) > 5 {
			t.Fatalf("a row of %d", len(bs))
		}
		for _, b := range bs {
			labels = append(labels, b["label"].(string))
		}
	}
	if len(labels) != 22 || labels[0] != "0" || labels[5] != "5" || labels[21] != "21" {
		t.Fatalf("labels %v", labels)
	}
	if strings.Contains(buf.String(), "buttons") {
		t.Fatalf("warned with none dropped: %s", buf.String())
	}
	rows = append(rows, rows...)
	if got := components(rows); len(got) != 5 || !strings.Contains(buf.String(), "44 buttons") {
		t.Fatalf("%d rows; log %q", len(got), buf.String())
	}
	if got := components(rows[:2]); len(got) != 2 {
		t.Fatalf("2 rows became %d", len(got))
	}
}

// Mentioning the bot's own role, as Discord's autocomplete often does,
// addresses it as a mention of the bot does, and is taken out of the
// text; another role's mention isn't.
func TestRoleMention(t *testing.T) {
	c := connected(newFake(t).client())
	c.events("GUILD_CREATE", json.RawMessage(`{"id":"`+guild+`","name":"Test server","channels":[{"id":"`+chanID+`","name":"general","type":0}],"threads":[],`+
		`"roles":[{"id":"700000000000000001","name":"everyone"},{"id":"700000000000000002","name":"Botty","tags":{"bot_id":"`+botID+`"}},{"id":"700000000000000003","name":"Other","tags":{"bot_id":"7"}}]}`))
	msg := func(content, roles string) bot.Received {
		return one(t, c.events("MESSAGE_CREATE", json.RawMessage(`{"id":"1200000000000000001",`+inGuild+`,`+fromA+`,"content":"`+content+`","mention_roles":[`+roles+`]}`)))
	}
	if m := load(t, msg("<@&700000000000000002> what is up", `"700000000000000002"`)); m.Text != "what is up" || !m.Addressed {
		t.Fatalf("its role: %+v", m)
	}
	if ch, ok := msg("<@&700000000000000003> hi", `"700000000000000003"`).(bot.Chatter); !ok || ch.Message.Text != "<@&700000000000000003> hi" {
		t.Fatalf("another bot's role: %+v", ch)
	}
	// A role made later, such as when the bot's is made again.
	c.events("GUILD_ROLE_CREATE", json.RawMessage(`{"guild_id":"`+guild+`","role":{"id":"700000000000000004","tags":{"bot_id":"`+botID+`"}}}`))
	if m := load(t, msg("hi <@&700000000000000004>", `"700000000000000004"`)); m.Text != "hi" || !m.Addressed {
		t.Fatalf("a new role: %+v", m)
	}
	c.events("GUILD_ROLE_DELETE", json.RawMessage(`{"guild_id":"`+guild+`","role_id":"700000000000000004"}`))
	if _, ok := msg("hi <@&700000000000000004>", `"700000000000000004"`).(bot.Chatter); !ok {
		t.Fatal("a deleted role still addresses the bot")
	}
}

// Delivered takes out the bot's @name and lists the people seen in the
// channel; Redact hides their names and Discord's IDs.
func TestDeliveredAndRedact(t *testing.T) {
	c := groupsClient(newFake(t))
	c.events("MESSAGE_CREATE", json.RawMessage(`{"id":"1200000000000000001",`+inGuild+`,`+fromBob+`,"content":"chatter"}`))
	r, ok := c.Delivered(context.Background(), feature.Message{Chat: feature.Chat{GroupID: group}, Sender: prefix + bob, Text: "@Botty hello"})
	if !ok || !r.Mentioned || r.Message.Text != "hello" || r.Message.Name != "Bob" || len(r.Members) != 1 || r.Members[0] != (bot.Member{Name: "Bob", ID: prefix + bob}) {
		t.Fatalf("Delivered %+v", r)
	}
	if got := c.Redact("Bob said 100000000000000002 to bobby"); got != "redacted said redacted to bobby" {
		t.Fatalf("Redact %q", got)
	}
}

// Owns is a Discord chat by its prefix.
func TestOwns(t *testing.T) {
	c := newFake(t).client()
	for chat, want := range map[feature.Chat]bool{
		{GroupID: group}: true, {User: prefix + alice}: true, {GroupID: "-1001234567890"}: false,
		{User: "1111111"}: false, {GroupID: "abc="}: false, {User: "slack:U1"}: false,
	} {
		if c.Owns(chat) != want {
			t.Errorf("Owns(%+v) = %v", chat, !want)
		}
	}
}

// Markdown becomes Discord's: nested styles, code blocks, escapes outside
// bare links.
func TestMarkdown(t *testing.T) {
	c := connected(newFake(t).client())
	for in, want := range map[string]string{
		"**bold *both* bold**":     "**bold *both* bold**",
		"~~gone~~ ||secret||":      "~~gone~~ ||secret||",
		"```\nfn main() {\n}\n```": "```\nfn main() {\n}\n```",
		"see https://x.io/a_b_c":   "see https://x.io/a_b_c",
		"snake_case and a|b":       "snake\\_case and a\\|b",
		"# Title":                  "**Title**",
		"@Botty hi":                "hi",
		"use `a`b` here":           "use `a`b\\` here",
		"[**x**](https://e.com/)":  "[**x**](https://e.com/)",
	} {
		ps := c.parts(in, nil, true)
		if len(ps) != 1 || ps[0].text != want {
			t.Errorf("%q: got %+v, want %q", in, ps, want)
		}
	}
}

// The channels and roles of TestGroups: #general, which everyone sees;
// #secret, which only the role sees, and a thread in it; a private thread
// in #general; a voice channel; and a second server.
const (
	roleID   = "600000000000000001"
	secret   = "400000000000000002"
	thread   = "400000000000000003"
	private  = "400000000000000004"
	voice    = "400000000000000005"
	guild2   = "300000000000000002"
	chanID2  = "400000000000000006"
	everyone = "274877910016" // VIEW_CHANNEL, SEND_MESSAGES and SEND_MESSAGES_IN_THREADS
	aliceGet = "GET /guilds/" + guild + "/members/" + alice
	bobGet   = "GET /guilds/" + guild + "/members/" + bob
)

// groupsClient is a client in TestGroups' servers, with Alice's and Bob's
// roles set on f: Bob has the role, Alice none.
func groupsClient(f *fake) *Client {
	f.results[aliceGet] = `{"roles":[]}`
	f.results[bobGet] = `{"roles":["` + roleID + `"]}`
	c := connected(f.client())
	c.events("GUILD_CREATE", json.RawMessage(`{"id":"`+guild+`","name":"Test server","owner_id":"700000000000000001",
		"roles":[{"id":"`+guild+`","permissions":"`+everyone+`"},{"id":"`+roleID+`","permissions":"0"}],
		"channels":[{"id":"`+chanID+`","name":"general","type":0},
			{"id":"`+secret+`","name":"secret","type":0,"permission_overwrites":[{"id":"`+guild+`","type":0,"allow":"0","deny":"1024"},{"id":"`+roleID+`","type":0,"allow":"1024","deny":"0"}]},
			{"id":"`+voice+`","name":"voice","type":2}],
		"threads":[{"id":"`+thread+`","name":"plans","type":11,"parent_id":"`+secret+`"},{"id":"`+private+`","name":"quiet","type":12,"parent_id":"`+chanID+`"}]}`))
	c.events("GUILD_CREATE", json.RawMessage(`{"id":"`+guild2+`","name":"Other server","owner_id":"700000000000000001",
		"roles":[{"id":"`+guild2+`","permissions":"`+everyone+`"}],"channels":[{"id":"`+chanID2+`","name":"lobby","type":0}]}`))
	return c
}

func groupNames(t *testing.T, c *Client, user string) []string {
	t.Helper()
	gs, err := c.Groups(context.Background(), prefix+user)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, g := range gs {
		out = append(out, g.Name)
	}
	return out
}

// Groups lists the channels a member can see by the server's roles and
// the channels' overwrites, a thread as its channel, a private thread only
// where they were seen, and none of a server they aren't in; each member
// is looked up once.
func TestGroups(t *testing.T) {
	f := newFake(t)
	c := groupsClient(f)
	c.events("MESSAGE_CREATE", json.RawMessage(`{"id":"1200000000000000001","guild_id":"`+guild+`","channel_id":"`+private+`",`+fromBob+`,"content":"hi"}`))
	c.events("MESSAGE_CREATE", json.RawMessage(`{"id":"1200000000000000002","guild_id":"`+guild2+`","channel_id":"`+chanID2+`",`+fromA+`,"content":"hi"}`))
	if got := groupNames(t, c, alice); !slices.Equal(got, []string{"#general (Test server)"}) {
		t.Fatalf("Alice's %q", got)
	}
	if got := groupNames(t, c, bob); !slices.Equal(got, []string{"#general (Test server)", "#plans (Test server)", "#quiet (Test server)", "#secret (Test server)"}) {
		t.Fatalf("Bob's %q", got)
	}
	groupNames(t, c, alice)
	if n, m := len(f.callsTo(aliceGet)), len(f.callsTo(bobGet)); n != 1 || m != 1 {
		t.Fatalf("looked up Alice %d times and Bob %d", n, m)
	}
	if n := len(f.callsTo("GET /guilds/" + guild2 + "/members/" + alice)); n != 1 {
		t.Fatalf("looked up Alice in the other server %d times", n)
	}

	// The owner sees every channel, as does an administrator.
	c.events("GUILD_UPDATE", json.RawMessage(`{"id":"`+guild+`","name":"Test server","owner_id":"`+alice+`"}`))
	if got := groupNames(t, c, alice); !slices.Equal(got, []string{"#general (Test server)", "#plans (Test server)", "#secret (Test server)"}) {
		t.Fatalf("the owner's %q", got)
	}
	c.events("GUILD_UPDATE", json.RawMessage(`{"id":"`+guild+`","owner_id":"700000000000000001"}`))
	c.events("GUILD_ROLE_UPDATE", json.RawMessage(`{"guild_id":"`+guild+`","role":{"id":"`+guild+`","permissions":"8"}}`))
	if got := groupNames(t, c, alice); len(got) != 3 {
		t.Fatalf("an administrator's %q", got)
	}

	// A member's own overwrite comes last.
	c.events("GUILD_ROLE_UPDATE", json.RawMessage(`{"guild_id":"`+guild+`","role":{"id":"`+guild+`","permissions":"`+everyone+`"}}`))
	c.events("CHANNEL_UPDATE", json.RawMessage(`{"id":"`+secret+`","guild_id":"`+guild+`","name":"secret","type":0,"permission_overwrites":[
		{"id":"`+guild+`","type":0,"allow":"0","deny":"1024"},{"id":"`+roleID+`","type":0,"allow":"1024","deny":"0"},{"id":"`+bob+`","type":1,"allow":"0","deny":"1024"}]}`))
	if got := groupNames(t, c, bob); !slices.Equal(got, []string{"#general (Test server)", "#quiet (Test server)"}) {
		t.Fatalf("Bob's, denied his own %q", got)
	}
}

// Groups lists only where a member may post: a read-only channel or an
// announcement channel only to those allowed to send there, a thread to
// those allowed to send in threads, and nothing while they are timed out,
// unless they own the server.
func TestGroupsPostOnly(t *testing.T) {
	const (
		rules = "400000000000000007"
		news  = "400000000000000008"
	)
	f := newFake(t)
	c := groupsClient(f)
	c.events("CHANNEL_CREATE", json.RawMessage(`{"id":"`+rules+`","guild_id":"`+guild+`","name":"rules","type":0,"permission_overwrites":[{"id":"`+guild+`","type":0,"allow":"0","deny":"2048"}]}`))
	c.events("CHANNEL_CREATE", json.RawMessage(`{"id":"`+news+`","guild_id":"`+guild+`","name":"news","type":5,"permission_overwrites":[{"id":"`+guild+`","type":0,"allow":"0","deny":"2048"},{"id":"`+roleID+`","type":0,"allow":"3072","deny":"0"}]}`))
	if got := groupNames(t, c, alice); !slices.Equal(got, []string{"#general (Test server)"}) {
		t.Fatalf("Alice's %q", got)
	}
	if got := groupNames(t, c, bob); !slices.Equal(got, []string{"#general (Test server)", "#news (Test server)", "#plans (Test server)", "#secret (Test server)"}) {
		t.Fatalf("Bob's %q", got)
	}

	// Not allowed to send in threads: #secret, not its thread.
	c.events("CHANNEL_UPDATE", json.RawMessage(`{"id":"`+secret+`","guild_id":"`+guild+`","name":"secret","type":0,"permission_overwrites":[
		{"id":"`+guild+`","type":0,"allow":"0","deny":"1024"},{"id":"`+roleID+`","type":0,"allow":"1024","deny":"274877906944"}]}`))
	if got := groupNames(t, c, bob); slices.Contains(got, "#plans (Test server)") || !slices.Contains(got, "#secret (Test server)") {
		t.Fatalf("Bob's, no threads %q", got)
	}

	// Timed out: nothing, until it ends; the owner is never timed out.
	f.results[aliceGet] = `{"roles":[],"communication_disabled_until":"` + time.Now().Add(time.Hour).Format(time.RFC3339) + `"}`
	c.mu.Lock()
	clear(c.members)
	c.mu.Unlock()
	if got := groupNames(t, c, alice); len(got) != 0 {
		t.Fatalf("timed out, Alice's %q", got)
	}
	c.events("GUILD_UPDATE", json.RawMessage(`{"id":"`+guild+`","name":"Test server","owner_id":"`+alice+`"}`))
	if got := groupNames(t, c, alice); len(got) == 0 {
		t.Fatal("the owner, timed out, has none")
	}
	c.events("GUILD_UPDATE", json.RawMessage(`{"id":"`+guild+`","name":"Test server","owner_id":"700000000000000001"}`))
	f.results[aliceGet] = `{"roles":[],"communication_disabled_until":"` + time.Now().Add(-time.Hour).Format(time.RFC3339) + `"}`
	c.mu.Lock()
	clear(c.members)
	c.mu.Unlock()
	if got := groupNames(t, c, alice); !slices.Equal(got, []string{"#general (Test server)"}) {
		t.Fatalf("after the timeout, Alice's %q", got)
	}
}

// A delivered post goes out only while its sender may still post in the
// channel: not once they left, lost the right to send, or can't be looked
// up; the log names no one.
func TestDeliveredChecksSender(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	c := groupsClient(f)
	deliver := func(ch, sender string) bool {
		_, ok := c.Delivered(context.Background(), feature.Message{Chat: feature.Chat{GroupID: prefix + ch}, Sender: prefix + sender, Text: "hello"})
		c.mu.Lock()
		clear(c.members)
		c.mu.Unlock()
		return ok
	}
	if !deliver(chanID, alice) || !deliver(secret, bob) {
		t.Fatal("not delivered for those who may post")
	}
	if deliver(secret, alice) {
		t.Fatal("delivered in a channel Alice can't see")
	}
	delete(f.results, aliceGet) // the fake's 404: she left
	if deliver(chanID, alice) {
		t.Fatal("delivered for someone who left")
	}
	f.results[aliceGet] = `{"roles":[]}`
	c.events("CHANNEL_UPDATE", json.RawMessage(`{"id":"`+chanID+`","guild_id":"`+guild+`","name":"general","type":0,"permission_overwrites":[{"id":"`+alice+`","type":1,"allow":"0","deny":"2048"}]}`))
	if deliver(chanID, alice) {
		t.Fatal("delivered for someone denied sending")
	}
	f.errs[bobGet] = []int{500}
	if deliver(secret, bob) {
		t.Fatal("delivered when the lookup failed")
	}
	if deliver(chanID, "not-an-id") || deliver("499999999999999999", bob) {
		t.Fatal("delivered for an unknown sender or channel")
	}
	if !strings.Contains(logs.String(), "can no longer post") || strings.Contains(logs.String(), alice) || strings.Contains(logs.String(), bob) || strings.Contains(logs.String(), guild) {
		t.Fatalf("log %q", logs.String())
	}
}

// When a member can't be looked up, Groups lists the channels they were
// seen in, and logs no one's ID.
func TestGroupsFallBack(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	c := groupsClient(f)
	f.errs[aliceGet] = []int{500}
	c.events("MESSAGE_CREATE", json.RawMessage(`{"id":"1200000000000000001","guild_id":"`+guild+`","channel_id":"`+private+`",`+fromA+`,"content":"hi"}`))
	if got := groupNames(t, c, alice); !slices.Equal(got, []string{"#quiet (Test server)"}) {
		t.Fatalf("Alice's %q", got)
	}
	if !strings.Contains(logs.String(), "only the channels they were seen in") || strings.Contains(logs.String(), alice) || strings.Contains(logs.String(), guild) {
		t.Fatalf("log %q", logs.String())
	}
	if got := groupNames(t, c, alice); !slices.Equal(got, []string{"#general (Test server)", "#quiet (Test server)"}) {
		t.Fatalf("Alice's once she can be looked up %q", got)
	}
	if got := groupNames(t, c, "not-an-id"); got != nil {
		t.Fatalf("a bad ID's %q", got)
	}
}

// members is Delivered's members in channel ch, as "Name ID" each.
func members(t *testing.T, c *Client, ch string) []string {
	t.Helper()
	r, ok := c.Delivered(context.Background(), feature.Message{Chat: feature.Chat{GroupID: prefix + ch}, Sender: prefix + bob, Text: "hello"})
	if !ok {
		t.Fatal("not delivered")
	}
	return memberNames(r.Members)
}

func memberNames(ms []bot.Member) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Name+" "+strings.TrimPrefix(m.ID, prefix))
	}
	return out
}

// listRoute is TestGroups' server's member list.
const listRoute = "GET /guilds/" + guild + "/members"

// listQuery is a page of the member list after the ID after, as the fake
// answers it.
func listQuery(after string) string { return listRoute + "?limit=1000&after=" + after }

// Refused the Server Members intent (4014), the client identifies again at
// once without it, warns once, and never asks for it again; refused again,
// it is the Message Content intent, and waits.
func TestMembersIntentRefused(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	c := f.client()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx, func(bot.Received) {}) }()
	defer func() { cancel(); <-done }()
	g := f.conn()
	if d, _ := g.next(t)["d"].(map[string]any); d["intents"].(float64) != intents {
		t.Fatalf("first identify %v", d)
	}
	g.CloseWith(4014, "Disallowed intent(s).")
	g = f.conn() // at once: the fatal wait is an hour
	id := g.next(t)
	if d, _ := id["d"].(map[string]any); id["op"] != float64(opIdentify) || int(d["intents"].(float64))&guildMembers != 0 || int(d["intents"].(float64))&(1<<15) == 0 {
		t.Fatalf("second identify %v", id)
	}
	g.CloseWith(4014, "Disallowed intent(s).")
	waitFor(t, "the line", func() bool { return strings.Contains(logs.String(), "Message Content Intent") })
	select {
	case <-f.conns:
		t.Fatal("reconnected at once after the second 4014")
	case <-time.After(100 * time.Millisecond):
	}
	if n := strings.Count(logs.String(), "Server Members Intent is off"); n != 1 {
		t.Fatalf("warned %d times:\n%s", n, logs)
	}
	if !c.membersOff.Load() {
		t.Fatal("the intent not remembered as off")
	}
}

// With the Server Members intent, Delivered's members are the server's
// members who can see the channel (a thread's, its parent's), but bots, a
// name seen winning over the list's; the list is read once in 10 minutes,
// by one caller while the others wait.
func TestDeliveredMembers(t *testing.T) {
	f := newFake(t)
	f.results[listQuery("0")] = `[{"user":{"id":"` + alice + `","username":"alice","global_name":"Alice"},"roles":[]},
		{"user":{"id":"` + bob + `","username":"bob"},"nick":"Bobby","roles":["` + roleID + `"]},
		{"user":{"id":"` + botID + `","username":"botty","bot":true},"roles":["` + roleID + `"]}]`
	c := groupsClient(f)
	hold := make(chan struct{})
	f.mu.Lock()
	f.hold[listRoute] = hold
	f.mu.Unlock()
	var wg sync.WaitGroup
	got := make([][]string, 3)
	for i := range got {
		wg.Go(func() {
			r, _ := c.Delivered(context.Background(), feature.Message{Chat: feature.Chat{GroupID: group}, Sender: prefix + bob, Text: "hello"})
			got[i] = memberNames(r.Members)
		})
	}
	f.waitCalls(listRoute, 1)
	time.Sleep(20 * time.Millisecond) // the others are waiting
	close(hold)
	wg.Wait()
	for _, g := range got {
		if !slices.Equal(g, []string{"Alice " + alice, "Bobby " + bob}) {
			t.Fatalf("#general's %q", g)
		}
	}
	if got := members(t, c, secret); !slices.Equal(got, []string{"Bobby " + bob}) {
		t.Fatalf("#secret's %q", got)
	}
	if got := members(t, c, thread); !slices.Equal(got, []string{"Bobby " + bob}) {
		t.Fatalf("the thread's %q", got)
	}
	// A private thread's members are only those seen there: the list
	// doesn't say who was added, and an @mention would add them.
	if got := members(t, c, private); len(got) != 0 {
		t.Fatalf("the private thread's %q", got)
	}
	c.events("MESSAGE_CREATE", json.RawMessage(`{"id":"1200000000000000002","guild_id":"`+guild+`","channel_id":"`+private+`",`+fromA+`,"content":"chatter"}`))
	if got := members(t, c, private); !slices.Equal(got, []string{"Alice " + alice}) {
		t.Fatalf("the private thread's once Alice is seen %q", got)
	}
	c.events("MESSAGE_CREATE", json.RawMessage(`{"id":"1200000000000000001",`+inGuild+`,`+fromBob+`,"content":"chatter"}`))
	if got := members(t, c, chanID); !slices.Equal(got, []string{"Alice " + alice, "Bob " + bob}) {
		t.Fatalf("#general's once Bob is seen %q", got)
	}
	if n := len(f.callsTo(listRoute)); n != 1 {
		t.Fatalf("listed %d times", n)
	}
	c.mu.Lock()
	c.lists[guild].at = time.Now().Add(-listTTL)
	c.mu.Unlock()
	members(t, c, chanID)
	if n := len(f.callsTo(listRoute)); n != 2 {
		t.Fatalf("listed %d times after 10 minutes", n)
	}
}

// The member list is read a page of 1,000 at a time, each after the last
// page's highest ID, at most 5 pages.
func TestDeliveredMembersPages(t *testing.T) {
	f := newFake(t)
	after := "0"
	for page := range 6 {
		var ms []string
		for i := range 1000 {
			ms = append(ms, fmt.Sprintf(`{"user":{"id":"%d","username":"b","bot":true}}`, 200000000000000000+page*1000+i))
		}
		switch page {
		case 1:
			ms[0] = `{"user":{"id":"` + bob + `","username":"bob","global_name":"Bob"}}`
		case 5:
			ms[0] = `{"user":{"id":"` + alice + `","username":"alice","global_name":"Alice"}}`
		}
		f.results[listQuery(after)] = "[" + strings.Join(ms, ",") + "]"
		after = fmt.Sprint(200000000000000000 + page*1000 + 999)
	}
	c := groupsClient(f)
	if got := members(t, c, chanID); !slices.Equal(got, []string{"Bob " + bob}) {
		t.Fatalf("members %q", got)
	}
	calls := f.callsTo(listRoute)
	if len(calls) != 5 || calls[1].Query != "limit=1000&after=200000000000000999" {
		t.Fatalf("calls %+v", calls)
	}
}

// Where the list can't be read, Delivered's members are the people seen:
// a 403 quietly, for 10 minutes, and another failure with one warning
// that names no one.
func TestDeliveredMembersFallBack(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	f.errs[listRoute] = []int{403}
	c := groupsClient(f)
	c.events("MESSAGE_CREATE", json.RawMessage(`{"id":"1200000000000000001",`+inGuild+`,`+fromBob+`,"content":"chatter"}`))
	for range 2 {
		if got := members(t, c, chanID); !slices.Equal(got, []string{"Bob " + bob}) {
			t.Fatalf("members %q", got)
		}
	}
	if n := len(f.callsTo(listRoute)); n != 1 || strings.Contains(logs.String(), "members") {
		t.Fatalf("listed %d times; log %q", n, logs.String())
	}

	f.errs[listRoute] = []int{500, 500}
	for range 2 {
		c.mu.Lock()
		c.lists[guild].at = time.Time{}
		c.mu.Unlock()
		if got := members(t, c, chanID); !slices.Equal(got, []string{"Bob " + bob}) {
			t.Fatalf("members %q", got)
		}
	}
	if n := strings.Count(logs.String(), "listing a server's members failed"); n != 1 || strings.Contains(logs.String(), guild) || strings.Contains(logs.String(), "Bob") {
		t.Fatalf("log %q", logs.String())
	}
}

// A Groups whose context ends fails, rather than warning for each server
// and listing only the channels seen.
func TestGroupsCanceled(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	c := groupsClient(f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if gs, err := c.Groups(ctx, prefix+alice); err == nil || gs != nil {
		t.Fatalf("Groups %v, %v", gs, err)
	}
	if strings.Contains(logs.String(), "only the channels") {
		t.Fatalf("log %q", logs.String())
	}
}

// Someone seen in a channel the bot loses keeps their name while they
// were seen in another channel; seen in none, they lose it.
func TestNamesPruned(t *testing.T) {
	f := newFake(t)
	c := connected(f.client())
	c.events("MESSAGE_CREATE", json.RawMessage(`{"id":"1200000000000000001",`+inGuild+`,`+fromBob+`,"content":"chatter"}`))
	c.events("MESSAGE_CREATE", json.RawMessage(`{"id":"1200000000000000002","guild_id":"`+guild+`","channel_id":"`+secret+`",`+fromBob+`,"content":"chatter"}`))
	c.events("MESSAGE_CREATE", json.RawMessage(`{"id":"1200000000000000003","guild_id":"`+guild+`","channel_id":"`+secret+`",`+fromA+`,"content":"chatter"}`))
	c.events("CHANNEL_DELETE", json.RawMessage(`{"id":"`+secret+`"}`))
	if got := c.Redact("Alice and Bob"); got != "Alice and redacted" {
		t.Fatalf("Redact %q", got)
	}
	c.mu.Lock()
	_, in := c.seenIn[alice]
	c.mu.Unlock()
	if in {
		t.Fatal("Alice still seen in a channel")
	}
	c.mu.Lock()
	bobIn := maps.Clone(c.seenIn[bob])
	c.mu.Unlock()
	if !maps.Equal(bobIn, map[string]bool{chanID: true}) {
		t.Fatalf("Bob seen in %v", bobIn)
	}
	c.mu.Lock()
	delete(c.seenIn, bob) // as if past seenIn's cap: only chats knows #general
	c.seenIn[bob] = map[string]bool{secret: true}
	c.chats[secret] = map[string]string{bob: "Bob"}
	c.leave(secret)
	bobIn = maps.Clone(c.seenIn[bob])
	c.mu.Unlock()
	if !maps.Equal(bobIn, map[string]bool{chanID: true}) || c.Redact("Bob") != "redacted" {
		t.Fatalf("Bob seen in %v, once found in #general's people", bobIn)
	}
	c.events("CHANNEL_DELETE", json.RawMessage(`{"id":"`+chanID+`"}`))
	if got := c.Redact("Bob"); got != "Bob" {
		t.Fatalf("Redact %q", got)
	}
}

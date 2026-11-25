package slack

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/allow"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/mdstyle"
	"github.com/robdevops/signalbot/internal/kit/ws/wstest"
)

const (
	token    = "xoxb-111-222-SECRETbot"
	appToken = "xapp-1-A111-SECRETapp"
	botID    = "UBOT00001"
	alice    = "UALICE001"
	bob      = "UBOB00001"
	channel  = "C0CHANNEL1"
)

func captureLogs(t *testing.T) *logx.Buffer {
	t.Helper()
	var buf logx.Buffer
	t.Cleanup(logx.SetOutput(&buf, &buf, false))
	t.Cleanup(func() { checkPrefixed(t, buf.String()) }) // runs first
	return &buf
}

// checkPrefixed fails t unless every line logged starts "Slack: ", after
// its priority, and names the network only there.
func checkPrefixed(t *testing.T, logs string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSuffix(logs, "\n"), "\n") {
		if line == "" {
			continue
		}
		rest, ok := strings.CutPrefix(strings.TrimLeft(line, "<01234567>"), "Slack: ")
		if !ok || strings.HasPrefix(strings.ToLower(rest), "slack") {
			t.Errorf("log line not prefixed once with %q: %q", "Slack: ", line)
		}
	}
}

// fakeSlack is the Web API and a Socket Mode server: each method answers
// with a response set for it, or an error once; /socket accepts the
// client's WebSocket and records its acknowledgements. Every call is
// recorded.
type fakeSlack struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	results map[string]string // method to a response's fields, without "ok"
	errs    map[string]string // method to an error, given once
	convs   map[string]string // users.conversations' answers, by "user cursor" (the bot's: no user)
	pages   map[string]string // conversations.members' and users.list's answers, by "method channel cursor"
	infos   map[string]string // users.info's answers, by user, instead of the usual ones
	calls   []call
	acks    []string
	opens   int
	conns   chan *wstest.Conn
	nextTS  int
}

type call struct {
	Method string
	Params map[string]any
}

func newFake(t *testing.T) *fakeSlack {
	f := &fakeSlack{t: t, results: map[string]string{
		"auth.test":          `"user_id":"UBOT00001","user":"botty"`,
		"conversations.open": `"channel":{"id":"D0ALICEDM1"}`,
	}, errs: map[string]string{}, conns: make(chan *wstest.Conn, 4)}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeSlack) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/socket":
		conn, err := wstest.Accept(w, r)
		if err != nil {
			return
		}
		f.conns <- conn
		for {
			msg, err := conn.Read()
			if err != nil {
				return
			}
			var ack struct {
				ID string `json:"envelope_id"`
			}
			json.Unmarshal([]byte(msg), &ack)
			f.mu.Lock()
			f.acks = append(f.acks, ack.ID)
			f.mu.Unlock()
		}
	case "/upload":
		b, _ := io.ReadAll(r.Body)
		f.record(call{Method: "upload", Params: map[string]any{"bytes": float64(len(b)), "auth": r.Header.Get("Authorization"), "type": r.Header.Get("Content-Type")}})
		return
	case "/files/x.png":
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "no", http.StatusForbidden)
			return
		}
		w.Write([]byte("PNGDATA"))
		return
	}
	method := strings.TrimPrefix(r.URL.Path, "/")
	c := call{Method: method, Params: map[string]any{}}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		json.NewDecoder(r.Body).Decode(&c.Params)
	} else {
		r.ParseForm()
		for k := range r.PostForm {
			c.Params[k] = r.PostForm.Get(k)
		}
	}
	want := "Bearer " + token
	if method == "apps.connections.open" {
		want = "Bearer " + appToken
	}
	if r.Header.Get("Authorization") != want {
		fmt.Fprint(w, `{"ok":false,"error":"invalid_auth"}`)
		return
	}
	f.record(c)
	f.mu.Lock()
	res, errCode := f.results[method], f.errs[method]
	delete(f.errs, method)
	switch method {
	case "apps.connections.open":
		f.opens++
		res = cmp.Or(res, `"url":"ws://`+r.Host+`/socket?ticket=SECRETticket"`)
	case "chat.postMessage":
		f.nextTS++
		res = fmt.Sprintf(`"ts":"1700000100.%06d"`, f.nextTS)
	case "files.getUploadURLExternal":
		f.nextTS++
		res = fmt.Sprintf(`"upload_url":"http://%s/upload","file_id":"F%08d"`, r.Host, f.nextTS)
	case "files.info":
		res = cmp.Or(res, `"file":{"url_private":"http://`+r.Host+`/files/x.png","size":7}`)
	case "users.conversations":
		var ok bool
		if res, ok = f.convs[r.PostForm.Get("user")+" "+r.PostForm.Get("cursor")]; !ok && r.PostForm.Get("user") == alice {
			res = `"channels":[{"id":"` + channel + `","name":"team"}]` // Alice is in the channel, unless a test says otherwise
		}
	case "conversations.members", "users.list":
		res = f.pages[method+" "+r.PostForm.Get("channel")+" "+r.PostForm.Get("cursor")]
	case "users.info":
		res = map[string]string{alice: `"user":{"name":"alice","profile":{"display_name":"Alice"}}`, bob: `"user":{"name":"bob","real_name":"Bob B"}`,
			botID: `"user":{"name":"botty","profile":{"display_name":"Botty"}}`}[r.PostForm.Get("user")]
		if v, ok := f.infos[r.PostForm.Get("user")]; ok {
			res = v
		}
	}
	f.mu.Unlock()
	switch {
	case errCode == "ratelimited":
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
	case errCode != "":
		fmt.Fprintf(w, `{"ok":false,"error":%q}`, errCode)
	case res != "":
		fmt.Fprintf(w, `{"ok":true,%s}`, res)
	default:
		fmt.Fprint(w, `{"ok":true}`)
	}
}

func (f *fakeSlack) record(c call) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
}

func (f *fakeSlack) callsTo(method string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []call
	for _, c := range f.calls {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeSlack) fail(method, code string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[method] = code
}

func (f *fakeSlack) client(t *testing.T) *Client {
	c := newClient(f.srv.URL, token, appToken, t.TempDir(), anyone, nil)
	c.backoff, c.slow, c.settle = 10*time.Millisecond, 50*time.Millisecond, time.Millisecond
	return c
}

// connected marks c as knowing who it is, as auth.test does.
func connected(c *Client) *Client {
	c.team.Store([2]string{"T1", ""})
	c.self.Store(botID)
	c.handle.Store("botty")
	c.name.Store("Botty")
	c.named.Store(true)
	c.once.Do(func() { close(c.known) })
	return c
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); !ok(); time.Sleep(2 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// ev is an events_api envelope holding the event e.
func ev(e string) envelope {
	return envelope{Type: "events_api", EnvelopeID: "e1", Payload: json.RawMessage(`{"event":` + e + `}`)}
}

func kindOf(t *testing.T, rs []bot.Received) (string, feature.Message) {
	t.Helper()
	if len(rs) == 0 {
		return "none", feature.Message{}
	}
	switch r := rs[0].(type) {
	case bot.Incoming:
		m, _ := r.Load(context.Background(), func(string, string) {})
		return "Incoming", m
	case bot.Chatter:
		return "Chatter", r.Message
	}
	return fmt.Sprintf("%T", rs[0]), feature.Message{}
}

func TestTimestamps(t *testing.T) {
	for _, ts := range []string{"1700000000.000200", "1700000000.123456", "1.000001"} {
		if got := tsString(tsInt(ts)); got != ts {
			t.Errorf("%s came back %s", ts, got)
		}
	}
	if tsInt("1700000000.000200") != 1700000000000200 || tsInt("1700000000.2") != 1700000000200000 || tsInt("x") != 0 {
		t.Error("tsInt")
	}
}

// Who the bot answers: anyone's DM, a mention, a message in a thread under
// one of its own; other channel messages are chatter; its own and other
// bots' are skipped.
func TestAddressing(t *testing.T) {
	c := connected(newFake(t).client(t))
	c.noteSent(channel, "1700000000.000100")
	c.learnName(bob, "Bob")
	for _, tc := range []struct {
		name, event, kind, text string
		chat                    feature.Chat
	}{
		{"mention", `{"type":"message","channel":"C0CHANNEL1","channel_type":"channel","user":"UALICE001","text":"<@UBOT00001> what is up","ts":"1700000000.000200"}`,
			"Incoming", "what is up", feature.Chat{GroupID: "slack:C0CHANNEL1"}},
		{"chatter", `{"type":"message","channel":"C0CHANNEL1","channel_type":"channel","user":"UALICE001","text":"ask <@UBOB00001> &amp; <https://x.example/a|the page>","ts":"1700000000.000300"}`,
			"Chatter", "ask @Bob & the page (https://x.example/a)", feature.Chat{GroupID: "slack:C0CHANNEL1"}},
		{"a DM", `{"type":"message","channel":"D0ALICEDM1","channel_type":"im","user":"UALICE001","text":"hi","ts":"1700000000.000400"}`,
			"Incoming", "hi", feature.Chat{User: "slack:UALICE001"}},
		{"in the bot's thread", `{"type":"message","channel":"C0CHANNEL1","channel_type":"channel","user":"UALICE001","text":"and?","ts":"1700000000.000500","thread_ts":"1700000000.000100"}`,
			"Incoming", "and?", feature.Chat{GroupID: "slack:C0CHANNEL1/1700000000.000100"}},
		{"in another thread", `{"type":"message","channel":"C0CHANNEL1","channel_type":"channel","user":"UALICE001","text":"and?","ts":"1700000000.000600","thread_ts":"1700000000.000050"}`,
			"Chatter", "and?", feature.Chat{GroupID: "slack:C0CHANNEL1/1700000000.000050"}},
		{"its own", `{"type":"message","channel":"C0CHANNEL1","user":"UBOT00001","text":"hi","ts":"1700000000.000700"}`, "none", "", feature.Chat{}},
		{"another bot", `{"type":"message","subtype":"bot_message","channel":"C0CHANNEL1","bot_id":"B1","text":"hi","ts":"1700000000.000800"}`, "none", "", feature.Chat{}},
		{"a join", `{"type":"message","subtype":"channel_join","channel":"C0CHANNEL1","user":"UALICE001","text":"joined","ts":"1700000000.000900"}`, "none", "", feature.Chat{}},
	} {
		kind, m := kindOf(t, c.events(ev(tc.event)))
		if kind != tc.kind || m.Text != tc.text || m.Chat != tc.chat {
			t.Errorf("%s: %s %q %+v", tc.name, kind, m.Text, m.Chat)
		}
	}
	if c.ims[alice] != "D0ALICEDM1" {
		t.Errorf("the DM's channel was not kept: %v", c.ims)
	}
}

// A message as the core sees it: sender, name fetched, files, and its ID.
func TestMessage(t *testing.T) {
	f := newFake(t)
	c := connected(f.client(t))
	rs := c.events(ev(`{"type":"message","subtype":"file_share","channel":"C0CHANNEL1","channel_type":"channel","user":"UALICE001","ts":"1700000000.000200",
		"text":"<@UBOT00001> look","files":[{"id":"F1","name":"a.png","mimetype":"image/png","size":100},{"id":"F2","name":"q3.pdf","mimetype":"application/pdf","size":2048}]}`))
	var learned []string
	m, _ := rs[0].(bot.Incoming).Load(context.Background(), func(n, id string) { learned = append(learned, n+" "+id) })
	want := feature.Message{Chat: feature.Chat{GroupID: "slack:" + channel}, Sender: "slack:" + alice, Name: "Alice", Text: "look", Timestamp: 1700000000000200, Addressed: true,
		Media: []feature.Media{{ID: "F1"}, {ID: "F2", File: true, Name: "q3.pdf", MIME: "application/pdf", Size: 2048}}}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("got  %+v\nwant %+v", m, want)
	}
	if !reflect.DeepEqual(learned, []string{"Alice slack:" + alice}) {
		t.Errorf("learned %v", learned)
	}
	data, err := c.Media(context.Background(), m, 0)
	if err != nil || string(data) != "PNGDATA" {
		t.Fatalf("media %q %v", data, err)
	}
}

// An edit is the message again, Edited later than it on the same scale; a
// change that only adds a link's preview is not an edit; a delete names
// the message.
func TestEditsAndDeletes(t *testing.T) {
	c := connected(newFake(t).client(t))
	_, m := kindOf(t, c.events(ev(`{"type":"message","subtype":"message_changed","channel":"D0ALICEDM1","channel_type":"im","ts":"1700000050.000001",
		"message":{"type":"message","user":"UALICE001","text":"hello there","ts":"1700000000.000200"},"previous_message":{"user":"UALICE001","text":"hello","ts":"1700000000.000200"}}`)))
	if m.Text != "hello there" || m.Timestamp != 1700000000000200 || m.Edited != 1700000050000001 || m.Chat.User != "slack:"+alice {
		t.Fatalf("edit %+v", m)
	}
	if rs := c.events(ev(`{"type":"message","subtype":"message_changed","channel":"C0CHANNEL1","channel_type":"channel","ts":"1700000050.000002",
		"message":{"user":"UALICE001","text":"see https://x","ts":"1700000000.000300"},"previous_message":{"user":"UALICE001","text":"see https://x","ts":"1700000000.000300"}}`)); len(rs) != 0 {
		t.Fatalf("an unfurl was an edit: %v", rs)
	}
	rs := c.events(ev(`{"type":"message","subtype":"message_deleted","channel":"C0CHANNEL1","channel_type":"channel","ts":"1700000060.000001","deleted_ts":"1700000000.000300",
		"previous_message":{"user":"UBOT00001","text":"x","ts":"1700000000.000300"}}`))
	if d, ok := rs[0].(bot.Deleted); !ok || d != (bot.Deleted{Chat: feature.Chat{GroupID: "slack:" + channel}, Sender: "slack:" + botID, Bot: true, Sent: 1700000000000300, At: 1700000060000001}) {
		t.Fatalf("delete %+v", rs)
	}
	// A thread's parent deleted is left as a placeholder by USLACKBOT: it
	// is a delete of every version, not an edit.
	for _, mark := range []string{`"subtype":"tombstone"`, `"hidden":true`} {
		rs = c.events(ev(`{"type":"message","subtype":"message_changed","hidden":true,"channel":"C0CHANNEL1","channel_type":"channel","ts":"1700000070.000001",
			"message":{"type":"message",` + mark + `,"user":"USLACKBOT","text":"This message was deleted.","ts":"1700000000.000400","thread_ts":"1700000000.000400"},
			"previous_message":{"user":"UALICE001","text":"a question","ts":"1700000000.000400","thread_ts":"1700000000.000400"}}`))
		if len(rs) != 1 || rs[0] != (bot.Deleted{Chat: feature.Chat{GroupID: "slack:" + channel}, Sender: "slack:" + alice, Sent: 1700000000000400, At: math.MaxInt64}) {
			t.Fatalf("%s: %+v", mark, rs)
		}
	}
}

// Reactions, with an event hook, by Slack's names; one the table doesn't
// know is skipped. The bot's own go out by name, or as a message.
func TestReactions(t *testing.T) {
	f := newFake(t)
	c := connected(f.client(t))
	e := `{"type":"reaction_added","user":"UALICE001","reaction":"thumbsup::skin-tone-2","item_user":"UBOT00001","item":{"type":"message","channel":"C0CHANNEL1","ts":"1700000000.000200"}}`
	if rs := c.events(ev(e)); len(rs) != 0 {
		t.Fatalf("without a hook: %v", rs)
	}
	c.watch.Store(true)
	rs := c.events(ev(e))
	h, ok := rs[0].(bot.Happened)
	if !ok || *h.Event.Reaction != (feature.Reaction{Emoji: "👍", Author: "slack:" + botID, Bot: true, Sent: 1700000000000200}) || h.Event.By.ID != "slack:"+alice {
		t.Fatalf("reaction %+v", rs)
	}
	// A workspace's own emoji is :name:, made safe and cut short.
	for name, want := range map[string]string{
		"custom_party":              ":custom_party:",
		"custom_party::skin-tone-3": ":custom_party:",
		strings.Repeat("long", 10):  ":" + strings.Repeat("long", 8) + "…:",
		"":                          "",
		"::skin-tone-2":             "",
	} {
		rs := c.events(ev(strings.Replace(e, "thumbsup::skin-tone-2", name, 1)))
		if want == "" {
			if len(rs) != 0 {
				t.Fatalf("%q: %v", name, rs)
			}
			continue
		}
		if len(rs) != 1 || rs[0].(bot.Happened).Event.Reaction.Emoji != want {
			t.Fatalf("%q: %+v", name, rs)
		}
	}
	// A reaction names only the channel and ts: one on a message in a
	// thread, the bot's or one it read there, is in the thread's chat.
	thread := feature.Chat{GroupID: "slack:" + channel + "/1700000000.000100"}
	sent := c.Send(thread, "in the thread", nil, nil, 0, nil, feature.Markup{})
	c.events(ev(`{"type":"message","channel":"C0CHANNEL1","channel_type":"channel","user":"UBOB00001","text":"me too","ts":"1700000000.000900","thread_ts":"1700000000.000100"}`))
	for _, ts := range []string{tsString(sent[0]), "1700000000.000900"} {
		rs := c.events(ev(strings.Replace(e, "1700000000.000200", ts, 1)))
		if len(rs) != 1 || rs[0].(bot.Happened).Event.Chat != thread {
			t.Fatalf("a reaction to %s in a thread: %+v", ts, rs)
		}
	}
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
	ch := feature.Chat{GroupID: "slack:" + channel}
	c.React(ch, feature.Message{Timestamp: 1700000000000200}, "❤")
	if r := f.callsTo("reactions.add"); len(r) != 1 || r[0].Params["name"] != "heart" || r[0].Params["timestamp"] != "1700000000.000200" || r[0].Params["channel"] != channel {
		t.Fatalf("reactions.add %v", r)
	}
	// One skin tone goes by Slack's name for it, or, if Slack refuses that
	// (an emoji that takes none), without it.
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
	c.React(ch, feature.Message{Timestamp: 1700000000000200}, "👍🏽")
	if r := f.callsTo("reactions.add"); len(r) != 1 || r[0].Params["name"] != "+1::skin-tone-4" {
		t.Fatalf("a skin tone %v", r)
	}
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
	f.fail("reactions.add", "invalid_name")
	c.React(ch, feature.Message{Timestamp: 1700000000000200}, "🦜🏽")
	if r := f.callsTo("reactions.add"); len(r) != 2 || r[0].Params["name"] != "parrot::skin-tone-4" || r[1].Params["name"] != "parrot" {
		t.Fatalf("a skin tone refused %v", r)
	}
	c.React(ch, feature.Message{Timestamp: 1700000000000200}, ":party_parrot:") // another network's own emoji, as it names it
	if p := f.callsTo("chat.postMessage"); len(p) != 1 || p[0].Params["text"] != ":party_parrot:" {
		t.Fatalf("an unknown emoji was not sent as a message: %v", p)
	}
}

// The bot leaving a channel changes it, and it no longer posts there.
func TestMembership(t *testing.T) {
	c := connected(newFake(t).client(t))
	ch := feature.Chat{GroupID: "slack:" + channel}
	if !c.Reachable(context.Background(), ch) || !c.Reachable(context.Background(), feature.Chat{User: "slack:" + alice}) {
		t.Fatal("not reachable")
	}
	rs := c.events(ev(`{"type":"member_left_channel","user":"UBOT00001","channel":"C0CHANNEL1"}`))
	if len(rs) != 1 || rs[0] != (bot.GroupChanged{Chat: ch}) || c.Reachable(context.Background(), ch) {
		t.Fatalf("left: %v", rs)
	}
	c.watch.Store(true)
	rs = c.events(ev(`{"type":"member_joined_channel","user":"UBOT00001","channel":"C0CHANNEL1","inviter":"UALICE001"}`))
	if len(rs) != 2 || !c.Reachable(context.Background(), ch) || !rs[1].(bot.Happened).Event.Members.BotAdded {
		t.Fatalf("joined: %v", rs)
	}
}

func TestOwns(t *testing.T) {
	c := newFake(t).client(t)
	for chat, want := range map[feature.Chat]bool{
		{GroupID: "slack:C1"}: true, {GroupID: "slack:C1/1700000000.000100"}: true, {User: "slack:U1"}: true,
		{GroupID: "-100123"}: false, {User: "+15550001111"}: false, {User: "1111111"}: false, {GroupID: "discord:1"}: false,
	} {
		if c.Owns(chat) != want {
			t.Errorf("Owns(%+v) is %v", chat, !want)
		}
	}
}

// Markdown becomes mrkdwn: styles, links, mentions of known names, code,
// and Slack's escapes.
func TestMrkdwn(t *testing.T) {
	known := map[string]string{"bob": "slack:" + bob}
	for md, want := range map[string]string{
		"**Yes**, see [the docs](https://example.com/d) & <x> @Bob": "*Yes*, see <https://example.com/d|the docs> &amp; &lt;x&gt; <@UBOB00001>",
		"an _italic_ and ~~gone~~ and `code`":                       "an _italic_ and ~gone~ and `code`",
		"```\nline 1\nline 2\n```":                                  "```\nline 1\nline 2\n```",
		"**bold with space **x":                                     "**bold with space **x",
	} {
		if got := mrkdwn(c0().trim(parse(md)), known); got != want {
			t.Errorf("%q: got %q, want %q", md, got, want)
		}
	}
}

// Send splits a long reply into sections within Slack's limit, puts
// pictures above the text and buttons under it, highlighted in the primary
// style, in a thread when the chat is one, and opens a DM's channel.
func TestSend(t *testing.T) {
	f := newFake(t)
	c := connected(f.client(t))
	long := strings.Repeat("word ", 1400) // 7,000 characters
	mk := feature.Markup{Buttons: [][]feature.Button{{{Label: "On", Data: "x:on", Highlight: true}, {Label: "Off", Data: "x:off"}}}}
	sent := c.Send(feature.Chat{GroupID: "slack:" + channel + "/1700000000.000100"}, long, []feature.Picture{{Data: []byte("png"), Name: "sky"}}, nil, 0, nil, mk)
	if len(sent) != 1 || sent[0] != tsInt("1700000100.000002") {
		t.Fatalf("sent %v", sent)
	}
	p := f.callsTo("chat.postMessage")[0].Params
	if p["channel"] != channel || p["thread_ts"] != "1700000000.000100" {
		t.Fatalf("posted to %v", p)
	}
	blocks := p["blocks"].([]any)
	var kinds []string
	for _, b := range blocks {
		m := b.(map[string]any)
		kinds = append(kinds, m["type"].(string))
		if m["type"] == "section" {
			if n := len([]rune(m["text"].(map[string]any)["text"].(string))); n > maxSection {
				t.Errorf("a section of %d", n)
			}
		}
	}
	if strings.Join(kinds, ",") != "image,section,section,section,actions" {
		t.Fatalf("blocks %v", kinds)
	}
	img := blocks[0].(map[string]any)
	if img["slack_file"].(map[string]any)["id"] != "F00000001" || img["alt_text"] != "sky" {
		t.Fatalf("image %v", img)
	}
	if u := f.callsTo("upload"); len(u) != 1 || u[0].Params["bytes"] != float64(3) || u[0].Params["auth"] != "" {
		t.Fatalf("upload %v", u)
	}
	if done := f.callsTo("files.completeUploadExternal"); len(done) != 1 || done[0].Params["channel_id"] != nil {
		t.Fatalf("complete %v", done)
	}
	btns, _ := json.Marshal(blocks[4].(map[string]any)["elements"])
	if !strings.Contains(string(btns), `"style":"primary","text":{"emoji":true,"text":"On","type":"plain_text"},"type":"button","value":"x:on"`) ||
		strings.Count(string(btns), "primary") != 1 {
		t.Fatalf("buttons %s", btns)
	}
	if !c.sentByBot(channel, "1700000100.000002") {
		t.Error("not remembered as the bot's")
	}

	c.Send(feature.Chat{User: "slack:" + alice}, "hi", nil, nil, 0, nil, feature.Markup{})
	if o := f.callsTo("conversations.open"); len(o) != 1 || o[0].Params["users"] != alice {
		t.Fatalf("open %v", o)
	}
	if p := f.callsTo("chat.postMessage"); p[1].Params["channel"] != "D0ALICEDM1" || p[1].Params["text"] != "hi" {
		t.Fatalf("DM %v", p[1].Params)
	}
	c.Send(feature.Chat{User: "slack:" + alice}, "again", nil, nil, 0, nil, feature.Markup{})
	if o := f.callsTo("conversations.open"); len(o) != 1 {
		t.Fatal("the DM's channel was not kept")
	}
}

// A picture is named and typed as what its data is: a JPEG goes as .jpg.
func TestSendJPEG(t *testing.T) {
	f := newFake(t)
	c := connected(f.client(t))
	c.Send(feature.Chat{GroupID: "slack:" + channel}, "a photo", []feature.Picture{{Data: []byte("\xff\xd8\xff\xe0\x00\x10JFIF\x00"), Name: "sky"}}, nil, 0, nil, feature.Markup{})
	if g := f.callsTo("files.getUploadURLExternal"); len(g) != 1 || g[0].Params["filename"] != "sky.jpg" {
		t.Fatalf("getUploadURLExternal %v", g)
	}
	if u := f.callsTo("upload"); len(u) != 1 || u[0].Params["type"] != "image/jpeg" {
		t.Fatalf("upload %v", u)
	}
	c.Send(feature.Chat{GroupID: "slack:" + channel}, "a clip", nil, nil, 0, []byte("mp3"), feature.Markup{})
	if u := f.callsTo("upload"); len(u) != 2 || u[1].Params["type"] != "audio/mpeg" {
		t.Fatalf("audio upload %v", u)
	}
}

// A picture Slack is still processing is tried again; a 429 waits.
func TestSendRetries(t *testing.T) {
	f := newFake(t)
	c := connected(f.client(t))
	f.fail("chat.postMessage", "invalid_blocks")
	if sent := c.Send(feature.Chat{GroupID: "slack:" + channel}, "a picture", []feature.Picture{{Data: []byte("png")}}, nil, 0, nil, feature.Markup{}); len(sent) != 1 {
		t.Fatalf("sent %v", sent)
	}
	if n := len(f.callsTo("chat.postMessage")); n != 2 {
		t.Fatalf("%d posts", n)
	}
	f.fail("chat.postMessage", "ratelimited")
	if sent := c.Send(feature.Chat{GroupID: "slack:" + channel}, "x", nil, nil, 0, nil, feature.Markup{}); len(sent) != 1 {
		t.Fatalf("after a 429: %v", sent)
	}
}

// An edit replaces the message in place; one with only buttons keeps its
// text.
func TestEdit(t *testing.T) {
	f := newFake(t)
	c := connected(f.client(t))
	ch := feature.Chat{GroupID: "slack:" + channel}
	sent := c.Send(ch, "first", nil, nil, 0, nil, feature.Markup{Buttons: [][]feature.Button{{{Label: "A", Data: "a"}}}})
	if got := c.Edit(ch, sent[0], "second", nil, nil, feature.Markup{}); !reflect.DeepEqual(got, sent) {
		t.Fatalf("edit returned %v", got)
	}
	u := f.callsTo("chat.update")[0].Params
	if u["ts"] != "1700000100.000001" || u["text"] != "second" || len(u["blocks"].([]any)) != 1 {
		t.Fatalf("update %v", u)
	}
	c.Edit(ch, sent[0], "", nil, nil, feature.Markup{Buttons: [][]feature.Button{{{Label: "B", Data: "b"}}}})
	u = f.callsTo("chat.update")[1].Params
	if b, _ := json.Marshal(u["blocks"]); u["text"] != "second" || !strings.Contains(string(b), `"value":"b"`) {
		t.Fatalf("buttons-only update %v", u)
	}
}

// Over the socket: every envelope is acknowledged; a press reaches the
// core and its answer is shown to the presser alone; a slash command is a
// message "/name args"; Slack's disconnect opens a new connection at once,
// and a dropped one is opened again after a wait.
func TestSocket(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	c := f.client(t)
	c.Watch(bot.Watch{Commands: []bot.Command{{Name: "hello", About: "About the bot"}}})
	got := make(chan bot.Received, 16)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx, func(r bot.Received) { got <- r }) }()
	defer func() { cancel(); <-done }()
	conn := <-f.conns
	conn.Write(`{"type":"hello"}`)
	waitFor(t, "connected", func() bool { return strings.Contains(logs.String(), "Slack: connected as @botty") })
	if c.WaitID(ctx) != "slack:"+botID || c.BotName() != "Botty" {
		t.Fatalf("who: %s %s", c.WaitID(ctx), c.BotName())
	}

	conn.Write(`{"type":"interactive","envelope_id":"p1","payload":{"type":"block_actions","user":{"id":"UALICE001","name":"alice"},"channel":{"id":"C0CHANNEL1"},
		"container":{"type":"message","message_ts":"1700000100.000001"},"message":{"ts":"1700000100.000001"},"actions":[{"action_id":"b0_0","value":"x:on"}]}}`)
	r := (<-got).(bot.Pressed)
	if r.Press != (feature.Press{Chat: feature.Chat{GroupID: "slack:" + channel}, By: feature.Person{ID: "slack:" + alice, Name: "alice"}, Sent: 1700000100000001, Data: "x:on"}) {
		t.Fatalf("press %+v", r.Press)
	}
	waitFor(t, "the press acknowledged", func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.acks) == 1 && f.acks[0] == "p1" })
	r.Answer("Alerts on")
	waitFor(t, "the answer", func() bool { return len(f.callsTo("chat.postEphemeral")) == 1 })
	if e := f.callsTo("chat.postEphemeral")[0].Params; e["user"] != alice || e["channel"] != channel || e["text"] != "Alerts on" {
		t.Fatalf("ephemeral %v", e)
	}

	conn.Write(`{"type":"slash_commands","envelope_id":"s1","payload":{"command":"/hello","text":"please","channel_id":"D0ALICEDM1","user_id":"UALICE001"}}`)
	in := (<-got).(bot.Incoming)
	m, _ := in.Load(ctx, func(string, string) {})
	if m.Text != "/hello please" || !m.Addressed || m.Chat != (feature.Chat{User: "slack:" + alice}) {
		t.Fatalf("command %+v", m)
	}

	conn.Write(`{"type":"disconnect","reason":"refresh_requested"}`)
	conn = <-f.conns
	conn.Write(`{"type":"hello"}`)
	conn.Drop()
	conn = <-f.conns
	f.mu.Lock()
	opens := f.opens
	f.mu.Unlock()
	if opens != 3 {
		t.Fatalf("%d connections opened", opens)
	}
	data, err := os.ReadFile(filepath.Join(c.stateDir, ManifestFile))
	if err != nil || !strings.Contains(string(data), `"command": "/hello"`) || !strings.Contains(string(data), `"description": "About the bot"`) ||
		!strings.Contains(string(data), `"name": "Botty"`) {
		t.Fatalf("manifest %s %v", data, err)
	}
	if !strings.Contains(logs.String(), "the commands changed: paste") {
		t.Fatalf("no notice:\n%s", logs.String())
	}
	if l := logs.String(); strings.Contains(l, "SECRET") {
		t.Fatalf("a token was logged:\n%s", l)
	}
}

// The manifest is written again only when it changes.
func TestManifestOnlyWhenChanged(t *testing.T) {
	logs := captureLogs(t)
	c := connected(newFake(t).client(t))
	c.Watch(bot.Watch{Commands: []bot.Command{{Name: "hello"}, {Name: "Not Slack's"}}})
	c.writeManifest()
	c.writeManifest()
	if n := strings.Count(logs.String(), "the commands changed"); n != 1 {
		t.Fatalf("%d notices", n)
	}
	var m manifest
	data, _ := os.ReadFile(filepath.Join(c.stateDir, ManifestFile))
	if json.Unmarshal(data, &m) != nil || len(m.Features.SlashCommands) != 1 || m.Features.SlashCommands[0] != (slashCommand{Command: "/hello", Description: "hello"}) ||
		!m.Settings.SocketModeEnabled || !m.Settings.Interactivity.IsEnabled {
		t.Fatalf("manifest %s", data)
	}
	// member_joined_channel and member_left_channel need these.
	for _, scope := range []string{"channels:read", "groups:read"} {
		if !slices.Contains(m.OAuthConfig.Scopes.Bot, scope) {
			t.Errorf("no %s in %v", scope, m.OAuthConfig.Scopes.Bot)
		}
	}
}

// A users.info that fails at the start doesn't rename the app: the name in
// the manifest already there stays, or none is written; the next hello
// asks again and writes it with the name.
func TestManifestNameUnknown(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	c := f.client(t)
	c.Watch(bot.Watch{Commands: []bot.Command{{Name: "hello"}}})
	path := filepath.Join(c.stateDir, ManifestFile)
	f.fail("users.info", "internal_error")
	if err := c.hello(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.writeManifest()
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a manifest was written without the bot's name")
	}
	c.rename(context.Background())
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), `"display_name": "Botty"`) {
		t.Fatalf("after a second try: %s", data)
	}
	if n := strings.Count(logs.String(), "the commands changed"); n != 1 {
		t.Fatalf("%d notices", n)
	}

	c = f.client(t)
	c.stateDir = filepath.Dir(path)
	c.Watch(bot.Watch{Commands: []bot.Command{{Name: "hello"}}})
	f.fail("users.info", "internal_error")
	if err := c.hello(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.writeManifest()
	if data, _ := os.ReadFile(path); !strings.Contains(string(data), `"display_name": "Botty"`) || strings.Contains(string(data), "botty") {
		t.Fatalf("renamed: %s", data)
	}
	if n := strings.Count(logs.String(), "the commands changed"); n != 1 {
		t.Fatalf("a false notice: %d", n)
	}
}

// -slack-manifest reads STATE_DIRECTORY's manifest when it is set, and
// otherwise the systemd unit's, Docker's or the working directory's, in
// turn, saying which files it tried when there is none.
func TestSavedManifest(t *testing.T) {
	unit, docker, state := t.TempDir(), t.TempDir(), t.TempDir()
	defer func(old []string) { manifestDirs = old }(manifestDirs)
	manifestDirs = []string{unit, docker}
	t.Setenv("STATE_DIRECTORY", "")
	data, path, tried := SavedManifest()
	if path != "" || !strings.Contains(string(data), `"name": "signalbot"`) ||
		!reflect.DeepEqual(tried, []string{filepath.Join(unit, ManifestFile), filepath.Join(docker, ManifestFile)}) {
		t.Fatalf("none: %q %v", path, tried)
	}
	os.WriteFile(filepath.Join(docker, ManifestFile), []byte("docker"), 0o600)
	if data, path, _ := SavedManifest(); string(data) != "docker" || path != filepath.Join(docker, ManifestFile) {
		t.Fatalf("docker: %q %q", data, path)
	}
	os.WriteFile(filepath.Join(unit, ManifestFile), []byte("unit"), 0o600)
	if data, path, _ := SavedManifest(); string(data) != "unit" || path != filepath.Join(unit, ManifestFile) {
		t.Fatalf("unit: %q %q", data, path)
	}
	t.Setenv("STATE_DIRECTORY", state)
	if _, path, tried := SavedManifest(); path != "" || !reflect.DeepEqual(tried, []string{filepath.Join(state, ManifestFile)}) {
		t.Fatalf("STATE_DIRECTORY: %q %v", path, tried)
	}
	os.WriteFile(filepath.Join(state, ManifestFile), []byte("state"), 0o600)
	if data, _, _ := SavedManifest(); string(data) != "state" {
		t.Fatalf("STATE_DIRECTORY: %q", data)
	}
}

// A Socket Mode connection that can't be opened is logged without its
// URL, which holds a ticket.
func TestDialErrorHidesTicket(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	closed := httptest.NewServer(http.NotFoundHandler())
	addr := closed.Listener.Addr().String()
	closed.Close()
	f.mu.Lock()
	f.results["apps.connections.open"] = `"url":"ws://` + addr + `/socket?ticket=SECRETticket"`
	f.mu.Unlock()
	c := connected(f.client(t))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx, func(bot.Received) {}) }()
	waitFor(t, "a failed dial", func() bool { return strings.Contains(logs.String(), "Slack: connection lost") })
	cancel()
	<-done
	if l := logs.String(); strings.Contains(l, "SECRET") || strings.Contains(l, "socket?") || !strings.Contains(l, "connection lost: socket: Get:") {
		t.Fatalf("log:\n%s", l)
	}
}

// A token Slack refuses is logged by its setting's name, never itself, and
// tried again slowly.
func TestRefusedToken(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	c := newClient(f.srv.URL, "xoxb-wrong-SECRET", appToken, t.TempDir(), anyone, nil)
	c.backoff, c.slow = time.Millisecond, 20*time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	c.Run(ctx, func(bot.Received) {})
	l := logs.String()
	if !strings.Contains(l, "Slack: refused SLACK_BOT_TOKEN (invalid_auth)") || strings.Contains(l, "SECRET") {
		t.Fatalf("log:\n%s", l)
	}
	if n := strings.Count(l, "refused"); n > 6 {
		t.Fatalf("retried fast: %d times", n)
	}
}

func TestCheck(t *testing.T) {
	t.Setenv("SLACK_BOT_TOKEN", "xoxb-1")
	t.Setenv("SLACK_APP_TOKEN", "")
	if err := Check(); err == nil || !strings.Contains(err.Error(), "SLACK_APP_TOKEN is not set") {
		t.Fatalf("no app token: %v", err)
	}
	t.Setenv("SLACK_APP_TOKEN", "xapp-1")
	if err := Check(); err != nil {
		t.Fatal(err)
	}
}

func TestRedact(t *testing.T) {
	c := connected(newFake(t).client(t))
	c.learnName(alice, "Alice Ames")
	if got := c.Redact("alice said hi to UBOB00001 in C0CHANNEL1"); got != "redacted said hi to redacted in redacted" {
		t.Fatalf("got %q", got)
	}
}

// Delivered takes the bot's @name out and gives the people seen in the
// channel as members.
func TestDelivered(t *testing.T) {
	c := connected(newFake(t).client(t))
	c.learnName(alice, "Alice")
	c.seen(channel, alice)
	r, ok := c.Delivered(context.Background(), feature.Message{Chat: feature.Chat{GroupID: "slack:" + channel}, Sender: "slack:" + alice, Text: "@botty hi"})
	if !ok || !r.Mentioned || r.Message.Text != "hi" || r.Message.Name != "Alice" || !reflect.DeepEqual(r.Members, []bot.Member{{Name: "Alice", ID: "slack:" + alice}}) {
		t.Fatalf("ready %+v", r)
	}
}

// rosterPages are a channel's members and the workspace's people, two
// pages of each: Bob, never seen posting, is in the channel, and a
// deleted user, a bot and Slackbot are skipped.
func rosterPages() map[string]string {
	return map[string]string{
		"conversations.members C0CHANNEL1 ":   `"members":["UALICE001","UDELETED1"],"response_metadata":{"next_cursor":"m2"}`,
		"conversations.members C0CHANNEL1 m2": `"members":["UBOB00001","UBOTUSER1","USLACKBOT","x>y"],"response_metadata":{"next_cursor":""}`,
		"users.list  ": `"members":[{"id":"UALICE001","name":"alice","profile":{"display_name":"Alice"}},` +
			`{"id":"UDELETED1","name":"gone","deleted":true,"profile":{"display_name":"Gone"}},` +
			`{"id":"UBOTUSER1","name":"helper","is_bot":true,"profile":{"display_name":"Helper"}}],"response_metadata":{"next_cursor":"u2"}`,
		"users.list  u2": `"members":[{"id":"UBOB00001","name":"bob","profile":{"display_name":"","real_name":"Bob"}},` +
			`{"id":"USLACKBOT","name":"slackbot","profile":{"display_name":"Slackbot"}}]`,
	}
}

// A delivered message's members are those Slack lists in its channel,
// every page read, named by the workspace's list, so @Bob links him though
// he was never seen; a thread's are its channel's.
func TestDeliveredMembers(t *testing.T) {
	f := newFake(t)
	f.pages = rosterPages()
	c := connected(f.client(t))
	chat := feature.Chat{GroupID: "slack:" + channel + "/1700000000.000100"}
	r, ok := c.Delivered(context.Background(), feature.Message{Chat: chat, Sender: "slack:" + alice, Name: "Alice", Text: "ask @Bob"})
	want := []bot.Member{{Name: "Alice", ID: "slack:" + alice}, {Name: "Bob", ID: "slack:" + bob}}
	if !ok || !reflect.DeepEqual(r.Members, want) {
		t.Fatalf("ready %+v", r)
	}
	if got := mrkdwn(c.trim(parse(r.Message.Text)), map[string]string{"bob": r.Members[1].ID}); got != "ask <@UBOB00001>" {
		t.Fatalf("got %q", got)
	}
	members, users := f.callsTo("conversations.members"), f.callsTo("users.list")
	if len(members) != 2 || members[0].Params["channel"] != channel || members[0].Params["limit"] != "200" || members[1].Params["cursor"] != "m2" {
		t.Fatalf("members calls %v", members)
	}
	if len(users) != 2 || users[0].Params["limit"] != "200" || users[1].Params["cursor"] != "u2" {
		t.Fatalf("users.list calls %v", users)
	}
	if len(f.callsTo("users.info")) != 0 {
		t.Fatal("asked users.info")
	}
}

// The lists are asked for once in 10 minutes, one fetch at a time, and
// again once that has passed; someone joining a channel asks for its
// members again.
func TestDeliveredMembersCached(t *testing.T) {
	f := newFake(t)
	f.pages = rosterPages()
	c := connected(f.client(t))
	msg := feature.Message{Chat: feature.Chat{GroupID: "slack:" + channel}, Sender: "slack:" + alice, Text: "hi"}
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			if r, ok := c.Delivered(context.Background(), msg); !ok || len(r.Members) != 2 {
				t.Errorf("ready %+v", r)
			}
		})
	}
	wg.Wait()
	count := func() (int, int) { return len(f.callsTo("conversations.members")), len(f.callsTo("users.list")) }
	if m, u := count(); m != 2 || u != 2 {
		t.Fatalf("calls %d %d", m, u)
	}

	c.roster.mu.Lock()
	for _, e := range c.roster.entries {
		e.at = e.at.Add(-11 * time.Minute)
	}
	c.roster.mu.Unlock()
	c.Delivered(context.Background(), msg)
	if m, u := count(); m != 4 || u != 4 {
		t.Fatalf("calls after 10 minutes %d %d", m, u)
	}

	c.events(ev(`{"type":"member_joined_channel","user":"UBOB00001","channel":"C0CHANNEL1"}`))
	c.Delivered(context.Background(), msg)
	if m, u := count(); m != 6 || u != 4 {
		t.Fatalf("calls after a join %d %d", m, u)
	}
}

// A chat whose channel isn't one Slack could list is never looked up, so
// the workspace's names, cached under "", are never taken for its members;
// Delivered drops a message for one.
func TestDeliveredMembersNotAChannel(t *testing.T) {
	f := newFake(t)
	f.pages = rosterPages()
	c := connected(f.client(t))
	if r, ok := c.Delivered(context.Background(), feature.Message{Chat: feature.Chat{GroupID: "slack:" + channel}, Sender: "slack:" + alice, Text: "hi"}); !ok || len(r.Members) != 2 {
		t.Fatalf("ready %+v", r)
	}
	for _, g := range []string{"slack:", "slack:/1700000000.000100", "slack:not-a-channel"} {
		if r, ok := c.Delivered(context.Background(), feature.Message{Chat: feature.Chat{GroupID: g}, Sender: "slack:" + alice, Text: "hi"}); ok {
			t.Fatalf("%s: delivered %+v", g, r)
		}
		ch, _ := c.channelOf(feature.Chat{GroupID: g})
		if ms := c.members(context.Background(), ch); len(ms) != 0 {
			t.Fatalf("%s: members %+v", g, ms)
		}
	}
	if n := len(f.callsTo("conversations.members")); n != 2 {
		t.Fatalf("members calls %d", n)
	}
}

// A delivered message goes out only while its sender is still in the
// channel, as Slack says; not if they left, nor if Slack can't say, and
// without the scopes to ask, the scopes are named once; the log names no
// one.
func TestDeliveredChecksSender(t *testing.T) {
	buf := captureLogs(t)
	f := newFake(t)
	c := connected(f.client(t))
	deliver := func(sender string) bool {
		_, ok := c.Delivered(context.Background(), feature.Message{Chat: feature.Chat{GroupID: "slack:" + channel + "/1700000000.000100"}, Sender: "slack:" + sender, Text: "hi"})
		return ok
	}
	if !deliver(alice) {
		t.Fatal("not delivered for a member")
	}
	if c := f.callsTo("users.conversations"); len(c) != 1 || c[0].Params["user"] != alice {
		t.Fatalf("calls %v", c)
	}
	if deliver(bob) {
		t.Fatal("delivered for someone not in the channel")
	}
	f.fail("users.conversations", "internal_error")
	if deliver(alice) {
		t.Fatal("delivered when Slack couldn't say")
	}
	for range 2 {
		f.fail("users.conversations", "missing_scope")
		if deliver(alice) {
			t.Fatal("delivered without the scopes to check")
		}
	}
	log := buf.String()
	if strings.Count(log, "channels:read") != 1 || !strings.Contains(log, "can no longer post") || strings.Contains(log, alice) || strings.Contains(log, bob) || strings.Contains(log, channel) {
		t.Fatalf("log %q", log)
	}
}

// A full roster prunes expired lists, but not one being fetched, so a
// second fetch of its key can't start alongside.
func TestRosterPruneKeepsBusy(t *testing.T) {
	var r roster
	r.entries = map[string]*rosterEntry{}
	for i := range maxRoster {
		r.entries[fmt.Sprint(i)] = &rosterEntry{busy: make(chan struct{}, 1), at: time.Now().Add(-time.Hour)}
	}
	busy := &rosterEntry{busy: make(chan struct{}, 1)}
	busy.busy <- struct{}{}
	r.entries["0"] = busy
	if _, err := r.get(context.Background(), "new", func(context.Context) (map[string]string, error) { return map[string]string{}, nil }); err != nil {
		t.Fatal(err)
	}
	if r.entries["0"] != busy || len(r.entries) != 2 {
		t.Fatalf("entries %d, busy kept %v", len(r.entries), r.entries["0"] == busy)
	}
}

// Without the scopes to list members, a delivered message's members are
// the people seen, and the scopes to add are logged once, naming no one;
// the manifest has them all.
func TestDeliveredMembersMissingScope(t *testing.T) {
	buf := captureLogs(t)
	f := newFake(t)
	f.pages = rosterPages()
	c := connected(f.client(t))
	c.learnName(alice, "Alice")
	c.seen(channel, alice)
	for _, method := range []string{"conversations.members", "users.list"} {
		f.fail(method, "missing_scope")
		r, ok := c.Delivered(context.Background(), feature.Message{Chat: feature.Chat{GroupID: "slack:" + channel}, Sender: "slack:" + alice, Text: "hi"})
		if !ok || !reflect.DeepEqual(r.Members, []bot.Member{{Name: "Alice", ID: "slack:" + alice}}) {
			t.Fatalf("ready %+v", r)
		}
	}
	log := buf.String()
	if strings.Count(log, "users:read") != 1 || !strings.Contains(log, "channels:read") || !strings.Contains(log, "groups:read") || strings.Contains(log, alice) || strings.Contains(log, "Alice") || strings.Contains(log, channel) {
		t.Fatalf("log %q", log)
	}
	m := string(Manifest("botty", nil))
	for _, scope := range []string{"channels:read", "groups:read", "users:read"} {
		if !strings.Contains(m, `"`+scope+`"`) {
			t.Fatalf("manifest lacks %s", scope)
		}
	}
}

// Groups are the channels both the user and the bot are in, by their
// names, sorted, every page read; the bot's own listing has no user.
func TestGroups(t *testing.T) {
	f := newFake(t)
	f.convs = map[string]string{
		" ":         `"channels":[{"id":"C0CHANNEL1","name":"team"},{"id":"C0CHANNEL2","name":"alpha\nnews"}],"response_metadata":{"next_cursor":"p2"}`,
		" p2":       `"channels":[{"id":"C0CHANNEL3","name":"zeta"}],"response_metadata":{"next_cursor":""}`,
		alice + " ": `"channels":[{"id":"C0CHANNEL3","name":"zeta"},{"id":"C0CHANNEL2","name":"alpha\nnews"},{"id":"C0CHANNEL4","name":"elsewhere"}]`,
	}
	c := connected(f.client(t))
	gs, err := c.Groups(context.Background(), "slack:"+alice)
	want := []feature.Group{{ID: "slack:C0CHANNEL2", Name: "#alpha news"}, {ID: "slack:C0CHANNEL3", Name: "#zeta"}}
	if err != nil || !reflect.DeepEqual(gs, want) {
		t.Fatalf("groups %+v %v", gs, err)
	}
	calls := f.callsTo("users.conversations")
	if len(calls) != 3 || calls[1].Params["cursor"] != "p2" || calls[0].Params["user"] != nil || calls[2].Params["user"] != alice {
		t.Fatalf("calls %v", calls)
	}
	for _, c := range calls {
		if c.Params["types"] != "public_channel,private_channel" || c.Params["exclude_archived"] != "true" || c.Params["limit"] != "200" {
			t.Fatalf("params %v", c.Params)
		}
	}
}

// Without the scopes to list channels, Groups lists those the user was
// seen in, by ID, and says once which scopes are missing, naming no one.
func TestGroupsMissingScope(t *testing.T) {
	buf := captureLogs(t)
	f := newFake(t)
	c := connected(f.client(t))
	c.learnName(alice, "Alice")
	c.seen(channel, alice)
	for range 2 {
		f.fail("users.conversations", "missing_scope")
		gs, err := c.Groups(context.Background(), "slack:"+alice)
		if err != nil || !reflect.DeepEqual(gs, []feature.Group{{ID: "slack:" + channel, Name: channel}}) {
			t.Fatalf("groups %+v %v", gs, err)
		}
	}
	log := buf.String()
	if strings.Count(log, "channels:read") != 1 || !strings.Contains(log, "groups:read") || strings.Contains(log, alice) || strings.Contains(log, "Alice") {
		t.Fatalf("log %q", log)
	}
}

// At startup the bot lists, in one warning, the channels it is in that
// are shared with another organisation, or invited to be, every page
// read, sorted by name: each as txt.LogName shows it, made safe, without
// control or format characters (an escape, a right-to-left override), cut
// to 40 characters, a backslash shown as "/". One shared only within its
// Enterprise Grid org isn't one.
func TestAuditChannels(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	long := strings.Repeat("x", 45)
	f.convs = map[string]string{
		" ": `"channels":[{"id":"C0CHANNEL1","name":"team"},{"id":"C0CHANNEL2","name":"partners\\","is_ext_shared":true},` +
			`{"id":"C0CHANNEL3","name":"grid","is_org_shared":true,"is_shared":true}],"response_metadata":{"next_cursor":"p2"}`,
		" p2": `"channels":[{"id":"C0CHANNEL4","name":"` + long + `","is_pending_ext_shared":true},{"id":"C0CHANNEL5","name":"a\nb","is_ext_shared":true},` +
			`{"id":"C0CHANNEL6","name":"\u001b[31mevil\u202eab","is_ext_shared":true}]`,
	}
	c := connected(f.client(t))
	c.auditChannels(context.Background())
	want := "Slack: the bot is in 4 channel(s) shared with other organisations, whose people can talk to it: " +
		"#[31mevilab (C0CHANNEL6), #a b (C0CHANNEL5), #partners/ (C0CHANNEL2), #" + strings.Repeat("x", 40) + "… (C0CHANNEL4)\n"
	if l := logs.String(); l != want {
		t.Fatalf("log %q", l)
	}
	if calls := f.callsTo("users.conversations"); len(calls) != 2 || calls[0].Params["user"] != nil || calls[1].Params["cursor"] != "p2" {
		t.Fatalf("calls %v", calls)
	}

	// None shared: a notice with the count alone.
	logs = captureLogs(t)
	f.convs = map[string]string{" ": `"channels":[{"id":"C0CHANNEL1","name":"team"},{"id":"C0CHANNEL3","name":"grid","is_org_shared":true}]`}
	c.auditChannels(context.Background())
	if l := logs.String(); l != "Slack: the bot is in 2 channel(s), none shared with other organisations\n" {
		t.Fatalf("log %q", l)
	}
}

// Without the scopes to list channels, the startup check says so once, as
// Groups would, naming no channel; another failure is a warning of its
// own.
func TestAuditChannelsFails(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	c := connected(f.client(t))
	for range 2 {
		f.fail("users.conversations", "missing_scope")
		c.auditChannels(context.Background())
	}
	f.fail("users.conversations", "missing_scope")
	c.Groups(context.Background(), "slack:"+alice)
	l := logs.String()
	if strings.Count(l, "Slack: refused to list channels (missing_scope)") != 1 || !strings.Contains(l, "aren't listed at startup") || strings.Count(l, "\n") != 1 {
		t.Fatalf("log %q", l)
	}
	logs = captureLogs(t)
	f.fail("users.conversations", "internal_error")
	c.auditChannels(context.Background())
	if l := logs.String(); !strings.HasPrefix(l, "Slack: users.conversations failed, so the channels shared with other organisations aren't listed: ") {
		t.Fatalf("log %q", l)
	}
}

// Run makes the startup check once it knows who the bot is, alongside the
// connection.
func TestAuditAtStartup(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	f.convs = map[string]string{" ": `"channels":[{"id":"C0CHANNEL2","name":"partners","is_ext_shared":true}]`}
	c := f.client(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx, func(bot.Received) {}) }()
	(<-f.conns).Write(`{"type":"hello"}`)
	waitFor(t, "the startup check", func() bool { return strings.Contains(logs.String(), "#partners (C0CHANNEL2)") })
	cancel()
	<-done
	if n := len(f.callsTo("users.conversations")); n != 1 {
		t.Fatalf("users.conversations asked %d times", n)
	}
}

// The file host check never sends the token elsewhere.
func TestMediaHost(t *testing.T) {
	f := newFake(t)
	c := connected(f.client(t))
	f.mu.Lock()
	f.results["files.info"] = `"file":{"url_private":"https://elsewhere.example/x.png","size":7}`
	f.mu.Unlock()
	if _, err := c.Media(context.Background(), feature.Message{Media: []feature.Media{{ID: "F1"}}}, 0); err == nil {
		t.Fatal("fetched from a host that isn't Slack's")
	}
}

func c0() *Client                  { return connected(newClient("http://x", token, appToken, "", anyone, nil)) }
func parse(md string) mdstyle.Text { return mdstyle.Parse(md, true) }

// anyone lets anyone DM the bot, as the tests but those of SLACK_DM_USERS do.
var anyone = allow.Parse("all")

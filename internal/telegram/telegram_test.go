package telegram

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

const (
	token  = "123456:SECRET-token-value"
	botID  = 777000
	alice  = 1111111
	bob    = 2222222
	group  = -1001234567890
	groupS = "-1001234567890"
)

func captureLogs(t *testing.T) *logx.Buffer {
	t.Helper()
	var buf logx.Buffer
	t.Cleanup(logx.SetOutput(&buf, &buf, false))
	return &buf
}

// fakeAPI is a Bot API: each method answers with a result set for it, or
// an error; getUpdates hands out the queued batches, then waits briefly
// and answers none. Every request is recorded.
type fakeAPI struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	results map[string]string // method to result JSON
	errs    map[string]string // method to a whole error response
	batches [][]any           // getUpdates' answers, in order
	calls   []call
	files   map[string][]byte // file path to its bytes
}

type call struct {
	Method string
	Params map[string]any
	Files  map[string][]byte
}

func newFake(t *testing.T) *fakeAPI {
	f := &fakeAPI{t: t, results: map[string]string{
		"getMe":         `{"id":777000,"is_bot":true,"first_name":"Botty","username":"botty_bot"}`,
		"deleteWebhook": `true`, "sendMessage": `{"message_id":500}`, "sendChatAction": `true`, "getChatMember": `{"status":"member"}`,
		"getChat": `{"id":-1001234567890,"type":"supergroup","message_auto_delete_time":86400}`,
	}, errs: map[string]string{}, files: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	if p, ok := strings.CutPrefix(r.URL.Path, "/file/bot"+token+"/"); ok {
		f.mu.Lock()
		data, found := f.files[p]
		f.mu.Unlock()
		if !found {
			http.NotFound(w, r)
			return
		}
		w.Write(data)
		return
	}
	method, ok := strings.CutPrefix(r.URL.Path, "/bot"+token+"/")
	if !ok {
		http.Error(w, `{"ok":false,"error_code":401,"description":"Unauthorized"}`, 401)
		return
	}
	c := call{Method: method, Params: map[string]any{}, Files: map[string][]byte{}}
	mt, ps, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt == "multipart/form-data" {
		mr := multipart.NewReader(r.Body, ps["boundary"])
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			data, _ := io.ReadAll(part)
			if part.FileName() != "" {
				c.Files[part.FormName()] = data
			} else {
				c.Params[part.FormName()] = string(data)
			}
		}
	} else {
		json.NewDecoder(r.Body).Decode(&c.Params)
	}
	f.mu.Lock()
	f.calls = append(f.calls, c)
	res, hasRes := f.results[method]
	errResp, hasErr := f.errs[method]
	if hasErr {
		delete(f.errs, method) // once
	}
	var batch []any
	if method == "getUpdates" && len(f.batches) > 0 {
		batch, f.batches = f.batches[0], f.batches[1:]
	}
	f.mu.Unlock()
	switch {
	case hasErr:
		w.Write([]byte(errResp))
	case method == "getUpdates":
		if batch == nil {
			select {
			case <-r.Context().Done():
			case <-time.After(50 * time.Millisecond):
			}
			batch = []any{}
		}
		b, _ := json.Marshal(batch)
		w.Write([]byte(`{"ok":true,"result":` + string(b) + `}`))
	case hasRes:
		w.Write([]byte(`{"ok":true,"result":` + res + `}`))
	default:
		w.Write([]byte(`{"ok":true,"result":true}`))
	}
}

func (f *fakeAPI) callsTo(method string) []call {
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

func (f *fakeAPI) client(allowed ...string) *Client {
	a := map[string]bool{}
	for _, id := range allowed {
		a[id] = true
	}
	c := newClient(f.srv.URL, token, a, nil)
	c.backoff, c.typing = 10*time.Millisecond, 20*time.Millisecond
	return c
}

// connected marks c as knowing who it is, as getMe does.
func connected(c *Client) *Client {
	c.self.Store("777000")
	c.username.Store("botty_bot")
	c.name.Store("Botty")
	c.once.Do(func() { close(c.known) })
	return c
}

func parse(t *testing.T, js string) update {
	t.Helper()
	var u update
	if err := json.Unmarshal([]byte(js), &u); err != nil {
		t.Fatal(err)
	}
	return u
}

const (
	inGroup = `"chat":{"id":-1001234567890,"type":"supergroup","title":"Test group"}`
	fromA   = `"from":{"id":1111111,"first_name":"Alice","last_name":"Ames"}`
	inDM    = `"chat":{"id":1111111,"type":"private"}`
)

// Who the bot answers: in a group, a mention of its @username or a text
// mention of it, a reply to it, or a command for it; in a DM, a listed
// sender. Its own mention and a command's @username are taken out.
func TestAddressing(t *testing.T) {
	logs := captureLogs(t)
	c := connected(newFake(t).client("1111111"))
	for _, tc := range []struct {
		name, msg, text string
		kind            string // Incoming, Chatter or none
	}{
		{"mention", `{"message_id":10,` + fromA + `,` + inGroup + `,"text":"@botty_bot what is up","entities":[{"type":"mention","offset":0,"length":10}]}`, "what is up", "Incoming"},
		{"mention mid", `{"message_id":10,` + fromA + `,` + inGroup + `,"text":"hey @Botty_Bot, hi","entities":[{"type":"mention","offset":4,"length":10}]}`, "hey , hi", "Incoming"},
		{"text mention", `{"message_id":10,` + fromA + `,` + inGroup + `,"text":"Botty hi","entities":[{"type":"text_mention","offset":0,"length":5,"user":{"id":777000,"first_name":"Botty"}}]}`, "hi", "Incoming"},
		{"command for it", `{"message_id":10,` + fromA + `,` + inGroup + `,"text":"/hello@botty_bot","entities":[{"type":"bot_command","offset":0,"length":16}]}`, "/hello", "Incoming"},
		{"bare command", `{"message_id":10,` + fromA + `,` + inGroup + `,"text":"/ping list","entities":[{"type":"bot_command","offset":0,"length":5}]}`, "/ping list", "Incoming"},
		{"another bot's command", `{"message_id":10,` + fromA + `,` + inGroup + `,"text":"/hello@other_bot","entities":[{"type":"bot_command","offset":0,"length":16}]}`, "/hello@other_bot", "Chatter"},
		{"reply to it", `{"message_id":10,` + fromA + `,` + inGroup + `,"text":"and?","reply_to_message":{"message_id":9,"from":{"id":777000,"is_bot":true,"first_name":"Botty"},` + inGroup + `,"text":"earlier"}}`, "and?", "Incoming"},
		{"chatter", `{"message_id":10,` + fromA + `,` + inGroup + `,"text":"the moon is cheese"}`, "the moon is cheese", "Chatter"},
		{"listed DM", `{"message_id":10,` + fromA + `,` + inDM + `,"text":"hi"}`, "hi", "Incoming"},
		{"unlisted DM", `{"message_id":10,"from":{"id":2222222,"first_name":"Bob"},"chat":{"id":2222222,"type":"private"},"text":"hi"}`, "", "none"},
		{"its own", `{"message_id":10,"from":{"id":777000,"is_bot":true,"first_name":"Botty"},` + inGroup + `,"text":"hi"}`, "", "none"},
	} {
		r, ok := c.event(parse(t, `{"update_id":1,"message":`+tc.msg+`}`))
		var text, kind string
		switch r := r.(type) {
		case bot.Incoming:
			m, _ := r.Load(context.Background(), func(string, string) {})
			text, kind = m.Text, "Incoming"
		case bot.Chatter:
			text, kind = r.Message.Text, "Chatter"
		default:
			kind = "none"
		}
		if (kind != "none") != ok || kind != tc.kind || text != tc.text {
			t.Errorf("%s: %s %q", tc.name, kind, text)
		}
	}
	if !strings.Contains(logs.String(), "2222222 ignored because not in TELEGRAM_DM_USER_IDS") {
		t.Errorf("logs %q", logs)
	}
}

// A message as the core sees it: sender, name, other people's text
// mentions as @Name (learned), the quote, photos, files and a sticker.
func TestMessage(t *testing.T) {
	c := connected(newFake(t).client("1111111"))
	u := parse(t, `{"update_id":1,"message":{"message_id":42,`+fromA+`,`+inGroup+`,"caption":"@botty_bot ask Bob 🎉 about this","caption_entities":[`+
		`{"type":"mention","offset":0,"length":10},{"type":"text_mention","offset":15,"length":3,"user":{"id":2222222,"first_name":"Bob"}}],`+
		`"photo":[{"file_id":"small","file_size":1000},{"file_id":"big","file_size":90000},{"file_id":"huge","file_size":99999999}],`+
		`"document":{"file_id":"doc1","file_name":"q3.pdf","mime_type":"application/pdf","file_size":2048},`+
		`"reply_to_message":{"message_id":41,"from":{"id":2222222,"first_name":"Bob"},`+inGroup+`,"text":"the whole thing","photo":[{"file_id":"qp","file_size":10}]},`+
		`"quote":{"text":"whole"},"sticker":{"file_id":"st","emoji":"🙂","set_name":"Pack"}}}`)
	r, _ := c.event(u)
	var learned []string
	m, _ := r.(bot.Incoming).Load(context.Background(), func(n, id string) { learned = append(learned, n+" "+id) })
	want := feature.Message{Chat: feature.Chat{GroupID: groupS}, Sender: "1111111", Name: "Alice Ames", Text: "ask @Bob 🎉 about this", Timestamp: 42, Addressed: true, Expires: 24 * time.Hour, // the chat's auto-delete timer
		Quote: &feature.Quote{Text: "whole", Author: "Bob", Sender: "2222222", Sent: 41},
		Media: []feature.Media{{ID: "big"}, {ID: "qp", Quoted: true}, {ID: "doc1", File: true, Name: "q3.pdf", MIME: "application/pdf", Size: 2048},
			{ID: "st", Sticker: true, Label: `: 🙂 from "Pack"`}}}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("got  %+v\nwant %+v", m, want)
	}
	if !reflect.DeepEqual(learned, []string{"Alice Ames 1111111", "Bob 2222222"}) {
		t.Errorf("learned %v", learned)
	}
}

// A location or venue is a Google Maps link, as Signal sends one, ahead of
// any caption; a live location, where its sender is now, is dropped.
func TestLocation(t *testing.T) {
	c := connected(newFake(t).client("1111111"))
	for _, tc := range []struct{ name, msg, text string }{
		{"location", `"location":{"latitude":-37.8136,"longitude":144.9631}`, "https://maps.google.com/maps?q=-37.8136,144.9631"},
		{"captioned", `"location":{"latitude":48.8584,"longitude":2.2945},"caption":"my guess"`, "https://maps.google.com/maps?q=48.8584,2.2945\nmy guess"},
		{"venue", `"venue":{"location":{"latitude":51.5,"longitude":-0.1275},"title":"A place"}`, "https://maps.google.com/maps?q=51.5,-0.1275"},
		{"live", `"location":{"latitude":-37.8136,"longitude":144.9631,"live_period":900}`, ""},
	} {
		r, ok := c.event(parse(t, `{"update_id":1,"message":{"message_id":10,`+fromA+`,`+inDM+`,`+tc.msg+`}}`))
		if tc.text == "" {
			if ok {
				t.Errorf("%s: kept %+v", tc.name, r)
			}
			continue
		}
		in, _ := r.(bot.Incoming)
		if !ok || in.Load == nil {
			t.Errorf("%s: dropped", tc.name)
			continue
		}
		if m, _ := in.Load(context.Background(), func(string, string) {}); m.Text != tc.text {
			t.Errorf("%s: %q", tc.name, m.Text)
		}
	}
}

// An edit is the message as it now reads, named by its message ID; in the
// DM of someone the bot doesn't answer it forgets the original.
func TestEdits(t *testing.T) {
	captureLogs(t)
	c := connected(newFake(t).client("1111111"))
	r, _ := c.event(parse(t, `{"update_id":1,"edited_message":{"message_id":42,`+fromA+`,`+inDM+`,"date":100,"edit_date":200,"text":"new words"}}`))
	m, _ := r.(bot.Incoming).Load(context.Background(), func(string, string) {})
	if m.Text != "new words" || m.Timestamp != 42 || m.Edited != 200 {
		t.Fatalf("edit %+v", m)
	}
	r, ok := c.event(parse(t, `{"update_id":2,"edited_message":{"message_id":7,"from":{"id":2222222,"first_name":"Bob"},"chat":{"id":2222222,"type":"private"},"date":100,"edit_date":300,"text":"x"}}`))
	if d, isDel := r.(bot.Deleted); !ok || !isDel || d != (bot.Deleted{Chat: feature.Chat{User: "2222222"}, Sender: "2222222", Sent: 7, At: 300}) {
		t.Fatalf("unlisted edit %+v", r)
	}
}

// Send writes the Markdown as entities in UTF-16 units, a link under its
// text, a known name as a text mention; a long reply is split at 4096.
func TestSend(t *testing.T) {
	f := newFake(t)
	c := connected(f.client())
	ids := c.Send(feature.Chat{GroupID: groupS}, "🎉 **big** news, see [docs](https://x.example) and @Alice", nil, map[string]string{"alice": "1111111"}, 0, nil, feature.Markup{})
	if !reflect.DeepEqual(ids, []int64{500}) {
		t.Fatalf("ids %v", ids)
	}
	p := f.callsTo("sendMessage")[0].Params
	if p["chat_id"] != groupS || p["text"] != "🎉 big news, see docs and @Alice" {
		t.Fatalf("params %v", p)
	}
	got, _ := json.Marshal(p["entities"])
	want := `[{"length":3,"offset":3,"type":"bold"},{"length":4,"offset":17,"type":"text_link","url":"https://x.example"},{"length":6,"offset":26,"type":"text_mention","user":{"id":1111111}}]`
	if string(got) != want {
		t.Fatalf("entities\n got  %s\n want %s", got, want)
	}
	c.Send(feature.Chat{User: "1111111"}, strings.Repeat("word ", 1700), nil, nil, 0, nil, feature.Markup{})
	if n := len(f.callsTo("sendMessage")); n != 3 { // 8500 chars, capped at 8000: two messages
		t.Fatalf("a long reply in %d messages", n-1)
	}
	for _, cl := range f.callsTo("sendMessage")[1:] {
		if s := cl.Params["text"].(string); len(s) > 4096 {
			t.Errorf("part of %d", len(s))
		}
	}
	// Entities Telegram refuses: the text alone.
	f.errs["sendMessage"] = `{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities"}`
	if ids := c.Send(feature.Chat{User: "1111111"}, "**x**", nil, nil, 0, nil, feature.Markup{}); len(ids) != 1 {
		t.Fatalf("fallback ids %v", ids)
	}
	if last := f.callsTo("sendMessage"); last[len(last)-1].Params["entities"] != nil || last[len(last)-1].Params["text"] != "x" {
		t.Errorf("fallback %v", last[len(last)-1].Params)
	}
}

// Pictures go first, the reply as the caption when it fits, an album for
// several.
func TestSendPictures(t *testing.T) {
	f := newFake(t)
	f.results["sendPhoto"] = `{"message_id":601}`
	f.results["sendMediaGroup"] = `[{"message_id":602},{"message_id":603}]`
	c := connected(f.client())
	png := []byte("\x89PNG\r\n\x1a\nxx")
	if ids := c.Send(feature.Chat{User: "1111111"}, "NVDA ▼ 15.2%", []feature.Picture{{Data: png}}, nil, 0, nil, feature.Markup{}); !reflect.DeepEqual(ids, []int64{601}) {
		t.Fatalf("ids %v", ids)
	}
	ph := f.callsTo("sendPhoto")[0]
	if ph.Params["caption"] != "NVDA ▼ 15.2%" || string(ph.Files["p0"]) != string(png) || len(f.callsTo("sendMessage")) != 0 {
		t.Fatalf("photo %v", ph.Params)
	}
	if ids := c.Send(feature.Chat{GroupID: groupS}, "", []feature.Picture{{Data: png}, {Data: png}}, nil, 0, nil, feature.Markup{}); !reflect.DeepEqual(ids, []int64{602, 603}) {
		t.Fatalf("album ids %v", ids)
	}
	if mg := f.callsTo("sendMediaGroup")[0]; len(mg.Files) != 2 || !strings.Contains(mg.Params["media"].(string), `"attach://p1"`) {
		t.Fatalf("album %v", mg.Params)
	}
}

// A reply's first message, and only its first, carries reply_parameters,
// sent anyway if the message it replies to is gone: a long reply's first
// part, or its pictures.
func TestSendReply(t *testing.T) {
	f := newFake(t)
	f.results["sendPhoto"] = `{"message_id":601}`
	c := connected(f.client())
	c.Send(feature.Chat{GroupID: groupS}, strings.Repeat("word ", 1700), nil, nil, 42, nil, feature.Markup{})
	msgs := f.callsTo("sendMessage")
	if len(msgs) != 2 {
		t.Fatalf("%d messages", len(msgs))
	}
	if got, _ := json.Marshal(msgs[0].Params["reply_parameters"]); string(got) != `{"allow_sending_without_reply":true,"message_id":42}` {
		t.Errorf("first part's reply_parameters %s", got)
	}
	if msgs[1].Params["reply_parameters"] != nil {
		t.Errorf("second part replies too: %v", msgs[1].Params["reply_parameters"])
	}
	png := []byte("\x89PNG\r\n\x1a\nxx")
	c.Send(feature.Chat{User: "1111111"}, strings.Repeat("x", maxCaption+1), []feature.Picture{{Data: png}}, nil, 43, nil, feature.Markup{})
	if ph := f.callsTo("sendPhoto")[0]; ph.Params["reply_parameters"] != `{"allow_sending_without_reply":true,"message_id":43}` {
		t.Errorf("photo's reply_parameters %v", ph.Params["reply_parameters"])
	}
	if msgs := f.callsTo("sendMessage"); len(msgs) != 3 || msgs[2].Params["reply_parameters"] != nil {
		t.Errorf("the text after the photo replies too: %v", msgs[len(msgs)-1].Params)
	}
	c.Send(feature.Chat{User: "1111111"}, "plain", nil, nil, 0, nil, feature.Markup{})
	if msgs := f.callsTo("sendMessage"); msgs[len(msgs)-1].Params["reply_parameters"] != nil {
		t.Errorf("a plain send replies: %v", msgs[len(msgs)-1].Params)
	}
}

// A reaction names its message by ID, the bot's own too; one Telegram
// refuses is sent as a message.
func TestReact(t *testing.T) {
	f := newFake(t)
	c := connected(f.client())
	msg := feature.Message{Chat: feature.Chat{User: "1111111"}, Timestamp: 42}
	c.React(msg.Chat, msg, "👍")
	if r := f.callsTo("setMessageReaction"); len(r) != 1 || r[0].Params["message_id"] != float64(42) {
		t.Fatalf("reactions %v", r)
	}
	own := feature.Message{Chat: msg.Chat, Timestamp: 43, Bot: true} // the bot's own, by its message ID
	c.React(own.Chat, own, "⏰")
	if r := f.callsTo("setMessageReaction"); len(r) != 2 || r[1].Params["message_id"] != float64(43) {
		t.Fatalf("reactions %v", r)
	}
	f.errs["setMessageReaction"] = `{"ok":false,"error_code":400,"description":"Bad Request: REACTION_INVALID"}`
	c.React(msg.Chat, msg, "🫠")
	if s := f.callsTo("sendMessage"); len(s) != 1 || s[0].Params["text"] != "🫠" {
		t.Fatalf("fallback %v", s)
	}
}

// Media is fetched by getFile and a download, and refused over the cap.
func TestMedia(t *testing.T) {
	f := newFake(t)
	f.results["getFile"] = `{"file_id":"big","file_path":"photos/a.jpg","file_size":3}`
	f.files["photos/a.jpg"] = []byte("JPG")
	c := connected(f.client())
	msg := feature.Message{Media: []feature.Media{{ID: "big"}, {ID: ""}}}
	if data, err := c.Media(context.Background(), msg, 0); err != nil || string(data) != "JPG" {
		t.Fatalf("got %q %v", data, err)
	}
	if _, err := c.Media(context.Background(), msg, 1); err == nil {
		t.Fatal("a file with no ID was fetched")
	}
	f.results["getFile"] = `{"file_id":"big","file_path":"photos/a.jpg","file_size":99999999}`
	if _, err := c.Media(context.Background(), msg, 0); err == nil {
		t.Fatal("a file over the cap was fetched")
	}
}

// Run learns who the bot is, drops a webhook, polls on from the last
// update, waits out a 429's retry_after, and hands on what it decodes.
func TestRun(t *testing.T) {
	captureLogs(t)
	f := newFake(t)
	f.batches = [][]any{{
		map[string]any{"update_id": 5, "message": json.RawMessage(`{"message_id":1,` + fromA + `,` + inDM + `,"text":"hi"}`)},
	}}
	c := f.client("1111111")
	got := make(chan bot.Received, 4)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx, func(r bot.Received) { got <- r }) }()
	select {
	case r := <-got:
		if _, ok := r.(bot.Incoming); !ok {
			t.Fatalf("got %T", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing handed on")
	}
	f.mu.Lock()
	f.errs["getUpdates"] = `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":1}}`
	f.mu.Unlock()
	time.Sleep(100 * time.Millisecond) // the next poll gets the 429
	before := len(f.callsTo("getUpdates"))
	time.Sleep(500 * time.Millisecond)
	if after := len(f.callsTo("getUpdates")); after != before {
		t.Errorf("%d polls within the 429's wait", after-before)
	}
	cancel()
	<-done
	if c.WaitID(context.Background()) != "777000" || len(f.callsTo("deleteWebhook")) != 1 {
		t.Fatalf("hello: %q, %d", c.selfID(), len(f.callsTo("deleteWebhook")))
	}
	polls := f.callsTo("getUpdates")
	if len(polls) < 2 || polls[0].Params["offset"] != float64(0) || polls[1].Params["offset"] != float64(6) {
		t.Fatalf("polls %v", polls)
	}
}

// The token is in every URL, so no error that reaches the log quotes it.
func TestTokenNeverLogged(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	c := f.client()
	f.srv.Close() // every call now fails with a URL error, which quotes the URL
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	c.Run(ctx, func(bot.Received) {})
	c.Send(feature.Chat{User: "1"}, "hi", nil, nil, 0, nil, feature.Markup{})
	out := logs.String()
	if !strings.Contains(out, "getMe failed") || !strings.Contains(out, "send failed") || strings.Contains(out, "SECRET") {
		t.Fatalf("logs %q", out)
	}
}

// Redact takes out the names the bot has seen and anything like an ID.
func TestRedact(t *testing.T) {
	c := connected(newFake(t).client())
	c.event(parse(t, `{"update_id":1,"message":{"message_id":1,`+fromA+`,`+inGroup+`,"text":"hi"}}`))
	if got := c.Redact("Alice's id 1111111 in -1001234567890 says hi"); got != "redacted's id redacted in redacted says hi" {
		t.Fatalf("got %q", got)
	}
}

// A delivered message in a group needs its sender still there; its members
// are the admins and the people seen posting; the bot's @name is taken
// out. In a DM, only a listed sender's.
func TestDelivered(t *testing.T) {
	captureLogs(t)
	f := newFake(t)
	f.results["getChatMember"] = `{"status":"member","user":{"id":1111111}}`
	f.results["getChatAdministrators"] = `[{"status":"creator","user":{"id":3333333,"first_name":"Carol"}},{"status":"administrator","user":{"id":777000,"is_bot":true,"first_name":"Botty"}}]`
	c := connected(f.client("1111111"))
	c.event(parse(t, `{"update_id":1,"message":{"message_id":1,`+fromA+`,`+inGroup+`,"text":"hi"}}`))
	r, ok := c.Delivered(context.Background(), feature.Message{Chat: feature.Chat{GroupID: groupS}, Sender: "1111111", Text: "@Botty remind us"})
	if !ok || !r.Mentioned || r.Message.Text != "remind us" || r.Message.Name != "Alice Ames" {
		t.Fatalf("got %+v %v", r, ok)
	}
	if !reflect.DeepEqual(r.Members, []bot.Member{{Name: "Carol", ID: "3333333"}, {Name: "Alice Ames", ID: "1111111"}}) {
		t.Errorf("members %v", r.Members)
	}
	f.results["getChatMember"] = `{"status":"left","user":{"id":1111111}}`
	if _, ok := c.Delivered(context.Background(), feature.Message{Chat: feature.Chat{GroupID: groupS}, Sender: "1111111", Text: "x"}); ok {
		t.Error("delivered for someone who left")
	}
	if _, ok := c.Delivered(context.Background(), feature.Message{Chat: feature.Chat{User: "2222222"}, Sender: "2222222", Text: "x"}); ok {
		t.Error("delivered in an unlisted DM")
	}
}

// The bot may post on its own in a listed user's DM, and in a group while
// someone listed is a member.
func TestReachable(t *testing.T) {
	captureLogs(t)
	f := newFake(t)
	c := connected(f.client("1111111"))
	ctx := context.Background()
	if !c.Reachable(ctx, feature.Chat{User: "1111111"}) || c.Reachable(ctx, feature.Chat{User: "2222222"}) {
		t.Fatal("DMs")
	}
	if !c.Reachable(ctx, feature.Chat{GroupID: groupS}) {
		t.Fatal("a group with Alice in it")
	}
	c.gate = map[string]gateState{}
	f.results["getChatMember"] = `{"status":"left"}`
	if c.Reachable(ctx, feature.Chat{GroupID: groupS}) {
		t.Fatal("a group no one listed is in")
	}
}

// Groups are those the bot has seen and the user is in; a group the bot
// was removed from is gone.
func TestGroups(t *testing.T) {
	f := newFake(t)
	f.results["getChatMember"] = `{"status":"member"}`
	c := connected(f.client())
	c.event(parse(t, `{"update_id":1,"message":{"message_id":1,`+fromA+`,`+inGroup+`,"text":"hi"}}`))
	gs, _ := c.Groups(context.Background(), "1111111")
	if !reflect.DeepEqual(gs, []feature.Group{{ID: groupS, Name: "Test group"}}) {
		t.Fatalf("groups %v", gs)
	}
	r, _ := c.event(parse(t, `{"update_id":2,"my_chat_member":{`+inGroup+`,"from":{"id":1111111,"first_name":"Alice"},"old_chat_member":{"status":"member"},"new_chat_member":{"status":"kicked","user":{"id":777000}}}}`))
	if _, ok := r.(bot.GroupChanged); !ok {
		t.Fatalf("removal %T", r)
	}
	if gs, _ := c.Groups(context.Background(), "1111111"); len(gs) != 0 {
		t.Fatalf("still listed %v", gs)
	}
}

// With an event hook, reactions and member changes are events: a reaction
// to the bot's own message says so; the bot added to a group is BotAdded.
func TestEvents(t *testing.T) {
	f := newFake(t)
	c := connected(f.client("1111111"))
	if _, ok := c.event(parse(t, `{"update_id":1,"message_reaction":{`+inGroup+`,"message_id":500,"user":{"id":1111111,"first_name":"Alice"},"old_reaction":[],"new_reaction":[{"type":"emoji","emoji":"👍"}]}}`)); ok {
		t.Fatal("an event with no hook")
	}
	c.Watch(bot.Watch{Events: true})
	c.groupAllowed(context.Background(), groupS)                                       // Alice is in it
	c.Send(feature.Chat{GroupID: groupS}, "hello", nil, nil, 0, nil, feature.Markup{}) // message 500, the bot's
	r, _ := c.event(parse(t, `{"update_id":2,"message_reaction":{`+inGroup+`,"message_id":500,"user":{"id":1111111,"first_name":"Alice"},"old_reaction":[{"type":"emoji","emoji":"👍"}],"new_reaction":[]}}`))
	e := r.(bot.Happened).Event
	if e.Reaction == nil || e.Reaction.Emoji != "👍" || !e.Reaction.Removed || !e.Reaction.Bot || e.By.ID != "1111111" || e.Reaction.Sent != 500 {
		t.Fatalf("reaction %+v %+v", e, e.Reaction)
	}
	r, _ = c.event(parse(t, `{"update_id":3,"my_chat_member":{`+inGroup+`,"from":{"id":1111111,"first_name":"Alice"},"old_chat_member":{"status":"left"},"new_chat_member":{"status":"member","user":{"id":777000}}}}`))
	if e := r.(bot.Happened).Event; e.Members == nil || !e.Members.BotAdded || e.By.ID != "1111111" {
		t.Fatalf("bot added %+v", e)
	}
	r, _ = c.event(parse(t, `{"update_id":4,"chat_member":{`+inGroup+`,"from":{"id":2222222,"first_name":"Bob"},"old_chat_member":{"status":"left"},"new_chat_member":{"status":"member","user":{"id":2222222,"first_name":"Bob"}}}}`))
	if e := r.(bot.Happened).Event; e.Members == nil || len(e.Members.Joined) != 1 || e.Members.Joined[0].Name != "Bob" || e.By.ID != "" {
		t.Fatalf("joined %+v", e)
	}
}

// Anyone can add a bot to a group of their own, so the bot answers in a
// group only while someone listed is a member, and leaves one where no one
// is: when it is added there, or a message arrives, however it got in.
func TestStrangersGroup(t *testing.T) {
	captureLogs(t)
	f := newFake(t)
	f.results["getChatMember"] = `{"status":"left"}`
	c := connected(f.client("1111111"))
	if _, ok := c.event(parse(t, `{"update_id":1,"my_chat_member":{`+inGroup+`,"from":{"id":2222222,"first_name":"Bob"},"old_chat_member":{"status":"left"},"new_chat_member":{"status":"member","user":{"id":777000}}}}`)); ok {
		t.Fatal("handed on")
	}
	waitCalls(t, f, "leaveChat", 1)
	if l := f.callsTo("leaveChat"); l[0].Params["chat_id"] != groupS {
		t.Fatalf("leaveChat %v", l)
	}
	// Still there, as the leave failed: an addressed message isn't
	// answered, and chatter isn't kept.
	r, ok := c.event(parse(t, `{"update_id":2,"message":{"message_id":3,"from":{"id":2222222,"first_name":"Bob"},`+inGroup+`,"text":"@botty_bot hi","entities":[{"type":"mention","offset":0,"length":10}]}}`))
	if !ok {
		t.Fatal("not handed on: checked in its slot")
	}
	if _, ok := r.(bot.Incoming).Load(context.Background(), func(string, string) {}); ok {
		t.Fatal("answered in a stranger's group")
	}
	if _, ok := c.event(parse(t, `{"update_id":3,"message":{"message_id":4,"from":{"id":2222222,"first_name":"Bob"},`+inGroup+`,"text":"chatter"}}`)); ok {
		t.Fatal("chatter kept")
	}
	if n := len(f.callsTo("leaveChat")); n != 1 {
		t.Fatalf("%d leaves within ten minutes", n)
	}
	// A restriction outlasts leaving: restricted and not a member is out.
	c.gate = map[string]gateState{}
	f.results["getChatMember"] = `{"status":"restricted","is_member":false}`
	if c.groupAllowed(context.Background(), groupS) {
		t.Fatal("a restricted non-member counted")
	}
	// Added by a listed user: answered at once, with no check.
	r, ok = c.event(parse(t, `{"update_id":4,"my_chat_member":{`+inGroup+`,"from":{"id":1111111,"first_name":"Alice"},"old_chat_member":{"status":"restricted","is_member":false},"new_chat_member":{"status":"member","user":{"id":777000}}}}`))
	if _, isGC := r.(bot.GroupChanged); !ok || !isGC {
		t.Fatalf("added by a listed user: %T %v", r, ok)
	}
	if allowed, known := c.gated(groupS); !allowed || !known {
		t.Fatal("not answered after a listed user added it")
	}
}

func waitCalls(t *testing.T, f *fakeAPI, method string, n int) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); len(f.callsTo(method)) < n; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("%d %s calls, want %d", len(f.callsTo(method)), method, n)
		}
	}
}

// When the pictures fail, the reply's text still goes; a caption whose
// entities Telegram refuses goes without them.
func TestSendPicturesFail(t *testing.T) {
	captureLogs(t)
	f := newFake(t)
	f.results["sendPhoto"] = `{"message_id":601}`
	c := connected(f.client())
	png := []byte("\x89PNG\r\n\x1a\nxx")
	f.errs["sendPhoto"] = `{"ok":false,"error_code":400,"description":"Bad Request: PHOTO_INVALID_DIMENSIONS"}`
	if ids := c.Send(feature.Chat{User: "1111111"}, "**the** caption", []feature.Picture{{Data: png}}, nil, 0, nil, feature.Markup{}); !reflect.DeepEqual(ids, []int64{500}) {
		t.Fatalf("ids %v", ids)
	}
	if s := f.callsTo("sendMessage"); len(s) != 1 || s[0].Params["text"] != "the caption" {
		t.Fatalf("text %v", s)
	}
	f.errs["sendPhoto"] = `{"ok":false,"error_code":400,"description":"Bad Request: can't parse entities"}`
	if ids := c.Send(feature.Chat{User: "1111111"}, "**the** caption", []feature.Picture{{Data: png}}, nil, 0, nil, feature.Markup{}); !reflect.DeepEqual(ids, []int64{601}) {
		t.Fatalf("ids %v", ids)
	}
	ph := f.callsTo("sendPhoto")
	if len(ph) != 3 || ph[2].Params["caption"] != "the caption" || ph[2].Params["caption_entities"] != "[]" || len(f.callsTo("sendMessage")) != 1 {
		t.Fatalf("photos %v", ph)
	}
}

// A send Telegram rate-limits is sent again after the wait it names; one to
// a group that became a supergroup goes to its new ID, as do later ones.
func TestSendRetries(t *testing.T) {
	captureLogs(t)
	f := newFake(t)
	c := connected(f.client())
	f.errs["sendMessage"] = `{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":1}}`
	start := time.Now()
	if ids := c.Send(feature.Chat{GroupID: groupS}, "hi", nil, nil, 0, nil, feature.Markup{}); !reflect.DeepEqual(ids, []int64{500}) || time.Since(start) < time.Second {
		t.Fatalf("ids %v after %s", ids, time.Since(start))
	}
	f.errs["sendMessage"] = `{"ok":false,"error_code":400,"description":"Bad Request: group chat was upgraded to a supergroup chat","parameters":{"migrate_to_chat_id":-1009999999999}}`
	c.Send(feature.Chat{GroupID: "-4444444"}, "one", nil, nil, 0, nil, feature.Markup{})
	c.Send(feature.Chat{GroupID: "-4444444"}, "two", nil, nil, 0, nil, feature.Markup{})
	var to []any
	for _, s := range f.callsTo("sendMessage")[2:] {
		to = append(to, s.Params["chat_id"])
	}
	if !reflect.DeepEqual(to, []any{"-4444444", "-1009999999999", "-1009999999999"}) {
		t.Fatalf("sent to %v", to)
	}
}

// A refusal other than of the emoji, such as of a deleted message, sends
// nothing in its place.
func TestReactRefused(t *testing.T) {
	captureLogs(t)
	f := newFake(t)
	c := connected(f.client())
	msg := feature.Message{Chat: feature.Chat{User: "1111111"}, Timestamp: 42}
	f.errs["setMessageReaction"] = `{"ok":false,"error_code":400,"description":"Bad Request: message to react not found"}`
	c.React(msg.Chat, msg, "👍")
	if s := f.callsTo("sendMessage"); len(s) != 0 {
		t.Fatalf("sent %v", s)
	}
}

// In a forum, a topic is a chat of its own: a message there is from
// "group/topic", isn't a quote of the message that made the topic, and the
// reply goes to the topic.
func TestTopics(t *testing.T) {
	f := newFake(t)
	c := connected(f.client("1111111"))
	r, _ := c.event(parse(t, `{"update_id":1,"message":{"message_id":20,"message_thread_id":7,"is_topic_message":true,`+fromA+`,`+inGroup+
		`,"text":"@botty_bot hi","entities":[{"type":"mention","offset":0,"length":10}],"reply_to_message":{"message_id":7,"from":{"id":3333333,"first_name":"Carol"},`+inGroup+`,"forum_topic_created":{"name":"Support"}}}}`))
	in, ok := r.(bot.Incoming)
	if !ok || in.Chat.GroupID != groupS+"/7" {
		t.Fatalf("got %T %+v", r, r)
	}
	m, _ := in.Load(context.Background(), func(string, string) {})
	if m.Quote != nil || m.Text != "hi" {
		t.Fatalf("message %+v", m)
	}
	c.Send(in.Chat, "hello", nil, nil, 0, nil, feature.Markup{})
	if s := f.callsTo("sendMessage")[0].Params; s["chat_id"] != groupS || s["message_thread_id"] != float64(7) {
		t.Fatalf("sent %v", s)
	}
	f.results["getChatMember"] = `{"status":"member"}`
	if gs, _ := c.Groups(context.Background(), "1111111"); len(gs) != 1 || gs[0].ID != groupS {
		t.Fatalf("groups %v", gs)
	}
	if _, ok := c.Delivered(context.Background(), feature.Message{Chat: in.Chat, Sender: "1111111", Text: "x"}); !ok {
		t.Fatal("not delivered in a topic")
	}
	if p := f.callsTo("getChatMember"); p[len(p)-1].Params["chat_id"] != groupS {
		t.Fatalf("getChatMember %v", p[len(p)-1].Params)
	}
}

// A group that became a supergroup is forgotten under its old ID and known
// under its new one.
func TestMigration(t *testing.T) {
	f := newFake(t)
	c := connected(f.client())
	c.event(parse(t, `{"update_id":1,"message":{"message_id":1,`+fromA+`,"chat":{"id":-4444444,"type":"group","title":"Old"},"text":"hi"}}`))
	r, _ := c.event(parse(t, `{"update_id":2,"message":{"message_id":2,`+fromA+`,"chat":{"id":-4444444,"type":"group","title":"Old"},"migrate_to_chat_id":-1009999999999}}`))
	if gc, ok := r.(bot.GroupChanged); !ok || gc.Chat.GroupID != "-4444444" {
		t.Fatalf("got %T %v", r, r)
	}
	f.results["getChatMember"] = `{"status":"member"}`
	if gs, _ := c.Groups(context.Background(), "1111111"); len(gs) != 1 || gs[0].ID != "-1009999999999" {
		t.Fatalf("groups %v", gs)
	}
	c.Send(feature.Chat{GroupID: "-4444444"}, "scheduled", nil, nil, 0, nil, feature.Markup{})
	if s := f.callsTo("sendMessage")[0].Params; s["chat_id"] != "-1009999999999" {
		t.Fatalf("sent to %v", s["chat_id"])
	}
}

// Updates handled when the bot stops are confirmed before it goes, so a
// restart doesn't get them again.
func TestRunConfirmsOnStop(t *testing.T) {
	f := newFake(t)
	f.batches = [][]any{{
		map[string]any{"update_id": 5, "message": json.RawMessage(`{"message_id":1,` + fromA + `,` + inDM + `,"text":"hi"}`)},
	}}
	c := f.client("1111111")
	ctx, cancel := context.WithCancel(context.Background())
	c.Run(ctx, func(bot.Received) { cancel() }) // stops while handling
	polls := f.callsTo("getUpdates")
	if last := polls[len(polls)-1].Params; len(polls) != 2 || last["offset"] != float64(6) || last["timeout"] != float64(0) {
		t.Fatalf("polls %v", polls)
	}
}

// Someone leaving a forum group changes each of its topics, as each is a
// chat of its own with its own names.
func TestTopicsGroupChanged(t *testing.T) {
	f := newFake(t)
	c := connected(f.client())
	c.events(parse(t, `{"update_id":1,"message":{"message_id":20,"message_thread_id":7,"is_topic_message":true,`+fromA+`,`+inGroup+`,"text":"hi"}}`))
	rs := c.events(parse(t, `{"update_id":2,"message":{"message_id":21,`+fromA+`,`+inGroup+`,"left_chat_member":{"id":2222222,"first_name":"Bob"}}}`))
	want := []bot.Received{bot.GroupChanged{Chat: feature.Chat{GroupID: groupS}}, bot.GroupChanged{Chat: feature.Chat{GroupID: groupS + "/7"}}}
	if !reflect.DeepEqual(rs, want) {
		t.Fatalf("got %v", rs)
	}
}

// When someone leaves, the bot asks again whether a listed user is still
// there, rather than answering on what it knew.
func TestLeaverRechecks(t *testing.T) {
	captureLogs(t)
	f := newFake(t)
	c := connected(f.client("1111111"))
	c.event(parse(t, `{"update_id":1,"my_chat_member":{`+inGroup+`,"from":{"id":1111111,"first_name":"Alice"},"old_chat_member":{"status":"left"},"new_chat_member":{"status":"member","user":{"id":777000}}}}`))
	f.mu.Lock()
	f.results["getChatMember"] = `{"status":"kicked"}`
	f.mu.Unlock()
	c.event(parse(t, `{"update_id":2,"message":{"message_id":5,"from":{"id":2222222,"first_name":"Bob"},`+inGroup+`,"left_chat_member":{"id":1111111,"first_name":"Alice"}}}`))
	waitCalls(t, f, "leaveChat", 1)
	r, _ := c.event(parse(t, `{"update_id":3,"message":{"message_id":6,"from":{"id":2222222,"first_name":"Bob"},`+inGroup+`,"text":"@botty_bot hi","entities":[{"type":"mention","offset":0,"length":10}]}}`))
	if _, ok := r.(bot.Incoming).Load(context.Background(), func(string, string) {}); ok {
		t.Fatal("answered after the listed user left")
	}
}

// With no one listed, every group is a no: the bot leaves, and keeps no
// chatter once it knows.
func TestNoOneListed(t *testing.T) {
	captureLogs(t)
	f := newFake(t)
	c := connected(f.client())
	c.event(parse(t, `{"update_id":1,"message":{"message_id":5,`+fromA+`,`+inGroup+`,"text":"hi"}}`))
	waitCalls(t, f, "leaveChat", 1)
	if _, ok := c.event(parse(t, `{"update_id":2,"message":{"message_id":6,`+fromA+`,`+inGroup+`,"text":"more"}}`)); ok {
		t.Fatal("chatter kept")
	}
}

// An event in a group the bot answers in still arrives after what it knew
// is out of date, and the group is checked again.
func TestEventsAfterExpiry(t *testing.T) {
	f := newFake(t)
	c := connected(f.client("1111111"))
	c.Watch(bot.Watch{Events: true})
	c.gate[groupS] = gateState{ok: true, until: time.Now().Add(-time.Minute)}
	r, ok := c.event(parse(t, `{"update_id":1,"message_reaction":{`+inGroup+`,"message_id":9,"user":{"id":1111111,"first_name":"Alice"},"old_reaction":[],"new_reaction":[{"type":"emoji","emoji":"👍"}]}}`))
	if _, isH := r.(bot.Happened); !ok || !isH {
		t.Fatalf("got %T %v", r, ok)
	}
	waitCalls(t, f, "getChatMember", 1)
}

// A GIF arrives as an animation, usually an MP4 with a still thumbnail:
// the model sees the thumbnail, not an unreadable file, and the same GIF
// in document isn't added again. A GIF kept as a GIF is shown itself.
func TestAnimation(t *testing.T) {
	c := connected(newFake(t).client("1111111"))
	for _, tc := range []struct {
		anim, want string
	}{
		{`{"file_id":"mp4","mime_type":"video/mp4","file_size":90000,"thumbnail":{"file_id":"still","file_size":4000}}`, "still"},
		{`{"file_id":"real","mime_type":"image/gif","file_size":30000,"thumbnail":{"file_id":"still","file_size":4000}}`, "real"},
	} {
		u := parse(t, `{"update_id":1,"message":{"message_id":7,`+fromA+`,`+inDM+`,"caption":"this is you",`+
			`"animation":`+tc.anim+`,"document":{"file_id":"doc","file_name":"idiot.mp4","mime_type":"video/mp4","file_size":90000}}}`)
		r, _ := c.event(u)
		m, _ := r.(bot.Incoming).Load(context.Background(), func(string, string) {})
		if !reflect.DeepEqual(m.Media, []feature.Media{{ID: tc.want}}) {
			t.Errorf("%s: media %+v", tc.want, m.Media)
		}
	}
	// One quoted: the quote's still, marked quoted.
	u := parse(t, `{"update_id":2,"message":{"message_id":8,`+fromA+`,`+inDM+`,"text":"what is this?","reply_to_message":{"message_id":7,`+fromA+`,`+inDM+
		`,"animation":{"file_id":"mp4","mime_type":"video/mp4","file_size":90000,"thumbnail":{"file_id":"still","file_size":4000}}}}}`)
	r, _ := c.event(u)
	m, _ := r.(bot.Incoming).Load(context.Background(), func(string, string) {})
	if !reflect.DeepEqual(m.Media, []feature.Media{{ID: "still", Quoted: true}}) {
		t.Errorf("quoted media %+v", m.Media)
	}
}

// An audio clip goes as a voice message, which shows no title, before the
// text, and it is the one that replies.
func TestSendVoice(t *testing.T) {
	f := newFake(t)
	f.results["sendVoice"] = `{"message_id":701}`
	c := connected(f.client())
	ids := c.Send(feature.Chat{GroupID: groupS}, "🎵 Song 1", nil, nil, 42, []byte("ID3 clip"), feature.Markup{})
	v := f.callsTo("sendVoice")
	if len(v) != 1 || v[0].Params["reply_parameters"] != `{"allow_sending_without_reply":true,"message_id":42}` {
		t.Fatalf("sendVoice %+v", v)
	}
	if msgs := f.callsTo("sendMessage"); len(msgs) != 1 || msgs[0].Params["reply_parameters"] != nil {
		t.Errorf("the text %+v", msgs)
	}
	if len(ids) != 2 || ids[0] != 701 {
		t.Errorf("ids %v", ids)
	}
}

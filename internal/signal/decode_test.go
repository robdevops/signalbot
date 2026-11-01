package signal

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/feature"
)

func parse(t *testing.T, s string) *envelope {
	t.Helper()
	var e envelope
	if err := json.Unmarshal([]byte(s), &e); err != nil {
		t.Fatal(err)
	}
	return &e
}

const (
	inGroup = `"groupInfo":{"groupId":"` + groupID + `"}`
	fromBob = `"sourceUuid":"` + bobID + `","sourceNumber":"+1555","sourceName":"Bob"`
	atBot   = `"mentions":[{"uuid":"` + botID + `","start":0,"length":1}]`
)

func TestAddressed(t *testing.T) {
	allowed := map[string]bool{"+1555": true}
	for _, tc := range []struct {
		name, env string
		want      bool
	}{
		{"mention", `{` + fromBob + `,"dataMessage":{"message":"￼ hi",` + atBot + `,` + inGroup + `}}`, true},
		{"mention by number", `{` + fromBob + `,"dataMessage":{"message":"￼ hi","mentions":[{"number":"+15550000","start":0,"length":1}],` + inGroup + `}}`, false},
		{"reply to the bot", `{` + fromBob + `,"dataMessage":{"message":"and?","quote":{"authorUuid":"` + botID + `"},` + inGroup + `}}`, true},
		{"reply to someone else", `{` + fromBob + `,"dataMessage":{"message":"and?","quote":{"authorUuid":"` + aliceID + `"},` + inGroup + `}}`, false},
		{"plain group message", `{` + fromBob + `,"dataMessage":{"message":"hi",` + inGroup + `}}`, false},
		{"DM from an allowed number", `{` + fromBob + `,"dataMessage":{"message":"hi"}}`, true},
		{"DM from anyone else", `{"sourceUuid":"` + aliceID + `","dataMessage":{"message":"hi"}}`, false},
		{"photo only DM", `{` + fromBob + `,"dataMessage":{"attachments":[{"contentType":"image/png","id":"a","size":10}]}}`, true},
		{"sticker only DM", `{` + fromBob + `,"dataMessage":{"sticker":{"packId":"p","stickerId":1}}}`, true},
		{"PDF only DM", `{` + fromBob + `,"dataMessage":{"attachments":[{"contentType":"application/pdf","id":"a","size":10}]}}`, false},
		{"empty DM", `{` + fromBob + `,"dataMessage":{"message":"  "}}`, false},
		{"no data message", `{` + fromBob + `}`, false},
		{"the bot's own", `{"sourceUuid":"` + botID + `","dataMessage":{"message":"hi"}}`, false},
	} {
		self := botID
		if got := isAddressed(parse(t, tc.env), self, allowed); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
		if isAddressed(parse(t, tc.env), "", allowed) {
			t.Errorf("%s: addressed before the bot's ID is known", tc.name)
		}
	}
}

func TestMessageInGroup(t *testing.T) {
	captureLogs(t)
	c := newClient("x", nil, nil)
	k := contact{UUID: aliceID}
	k.Profile.GivenName = "Alice"
	c.names.replace([]contact{k})
	// "@bot ask @Alice and @someone 🎉", replying to the bot with a photo.
	e := parse(t, `{`+fromBob+`,"dataMessage":{"message":"￼ ask ￼ and ￼ 🎉","expiresInSeconds":3600,`+
		`"mentions":[{"uuid":"`+botID+`","start":0,"length":1},{"uuid":"unknown","name":"unknown","start":12,"length":1},{"uuid":"`+aliceID+`","name":"`+aliceID+`","start":6,"length":1}],`+
		`"attachments":[{"contentType":"image/jpeg","id":"a1","size":10},{"contentType":"image/png","id":"big","size":`+"99999999"+`,"thumbnail":{"contentType":"image/png","id":"t1","size":5}},{"contentType":"text/plain","id":"x","size":1}],`+
		`"quote":{"authorUuid":"`+botID+`","text":"earlier","attachments":[{"contentType":"image/png","id":"q1","size":5},{"contentType":"image/png","id":"q2","size":5},{"contentType":"image/png","id":"q3","size":5}]},`+
		`"sticker":{"packId":"p","stickerId":2},`+inGroup+`}}`)
	m := c.message(e, botID)
	want := feature.Message{
		Chat: feature.Chat{GroupID: groupID}, Sender: bobID, Name: "Bob",
		Text:    "ask @Alice and [unnamed user] 🎉",
		Quote:   &feature.Quote{Text: "earlier", Bot: true},
		Expires: time.Hour, Addressed: true,
		Media: []feature.Media{{ID: "a1"}, {ID: "t1"},
			{ID: "q1", Quoted: true}, {ID: "q2", Quoted: true}, {ID: "p:2", Sticker: true}},
	}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("got  %+v\nwant %+v", m, want)
	}
	if people := people(c, e); !reflect.DeepEqual(people, []string{"Bob " + bobID, "Alice " + aliceID}) {
		t.Fatalf("people %+v", people)
	}
}

func TestMessageInDM(t *testing.T) {
	c := newClient("x", map[string]bool{aliceID: true}, nil)
	e := parse(t, `{"sourceUuid":"`+aliceID+`","sourceName":"Alice\nSmith","dataMessage":{"message":"hi","quote":{"authorUuid":"`+bobID+`","text":"x"}}}`)
	m := c.message(e, botID)
	if m.Chat != (feature.Chat{User: aliceID}) || m.Name != "Alice Smith" || !m.Addressed || m.Quote.Author != "" || m.Quote.Bot {
		t.Fatalf("got %+v %+v", m, m.Quote)
	}
	if p := people(c, e); p != nil {
		t.Fatalf("a DM showed people: %v", p)
	}
	c.names.replace([]contact{{UUID: bobID, GivenName: "Bob"}})
	delete(c.allowed, aliceID)
	if m := c.message(e, botID); m.Quote.Author != "Bob" || m.Addressed {
		t.Fatalf("got %+v %+v", m, m.Quote)
	}
}

// Mentions out of order, overlapping or out of range never splice over the
// text.
func TestMentionsOddOffsets(t *testing.T) {
	captureLogs(t)
	c := newClient("x", nil, nil)
	for _, tc := range []struct {
		ms   []mention
		want string
	}{
		{[]mention{{UUID: botID, Start: 0, Length: 1}, {UUID: "u", Name: "Bob", Start: 4, Length: 1}}, "a @Bob"},
		{[]mention{{UUID: "u", Name: "Bob", Start: 4, Length: 1}, {UUID: botID, Start: 0, Length: 1}}, "a @Bob"},
		{[]mention{{UUID: "u", Name: "Bob", Start: 0, Length: 4}, {UUID: botID, Start: 2, Length: 2}}, "￼ ￼"},
		{[]mention{{UUID: "u", Start: 99, Length: 1}, {UUID: "u", Start: 1, Length: -1}}, "￼ a ￼"},
	} {
		if got := c.text("￼ a ￼", tc.ms, botID); got != tc.want {
			t.Errorf("%v: %q", tc.ms, got)
		}
	}
}

// An unresolved mention is shown as [unnamed user], and never logged,
// addressed or not.
func TestUnresolvedMentionNotLogged(t *testing.T) {
	logs := captureLogs(t)
	c := newClient("x", nil, nil)
	unknown := `"mentions":[{"uuid":"` + aliceID + `","start":0,"length":1}]`
	if m := c.message(parse(t, `{`+fromBob+`,"dataMessage":{"message":"￼ hi",`+unknown+`,`+inGroup+`}}`), botID); m.Text != "[unnamed user] hi" {
		t.Fatalf("text %q", m.Text)
	}
	both := `"mentions":[{"uuid":"` + botID + `","start":0,"length":1},{"uuid":"` + aliceID + `","start":2,"length":1}]`
	if m := c.message(parse(t, `{`+fromBob+`,"dataMessage":{"message":"￼ ￼",`+both+`,`+inGroup+`}}`), botID); m.Text != "[unnamed user]" {
		t.Fatalf("text %q", m.Text)
	}
	if logs.String() != "" {
		t.Fatalf("logged %q", logs.String())
	}
}

// people is what c.people passes learn, as "name id".
func people(c *Client, e *envelope) []string {
	var out []string
	c.people(e, botID, func(name, id string) { out = append(out, name+" "+id) })
	return out
}

// connected marks c as knowing its own ID, as listAccounts does.
func connected(c *Client) {
	c.id.Store(botID)
	c.knownOnce.Do(func() { close(c.known) })
}

// On the reader, an addressed message waits to be loaded in its slot, group
// chatter is decoded for the hooks, a group change is only that, and the
// bot's own messages and a stranger's DM reach the core not at all; the
// stranger is logged by ID only.
func TestEvents(t *testing.T) {
	logs := captureLogs(t)
	c := newClient("x", map[string]bool{aliceID: true}, nil)
	c.closed.Store(true) // so a group change schedules no contact load
	connected(c)
	event := func(env string) (bot.Received, bool) { return c.event(parse(t, env)) }

	if r, ok := event(`{` + fromBob + `,"dataMessage":{"message":"￼ hi",` + atBot + `,` + inGroup + `}}`); !ok ||
		!reflect.DeepEqual(r.Message, feature.Message{Chat: feature.Chat{GroupID: groupID}, Addressed: true}) || r.Load == nil || r.GroupChange {
		t.Errorf("mention %+v", r)
	}
	if r, ok := event(`{` + fromBob + `,"dataMessage":{"message":"the moon",` + inGroup + `}}`); !ok ||
		r.Text != "the moon" || r.Name != "Bob" || r.Addressed || r.Load != nil {
		t.Errorf("chatter %+v", r)
	}
	update := `{"sourceUuid":"` + botID + `","dataMessage":{"groupInfo":{"groupId":"` + groupID + `","type":"UPDATE"}}}`
	if r, ok := event(update); !ok || !r.GroupChange || r.Chat.GroupID != groupID || r.Load != nil {
		t.Errorf("group change %+v", r)
	}
	for _, env := range []string{
		`{"sourceUuid":"` + botID + `","dataMessage":{"message":"hi",` + inGroup + `}}`,
		`{` + fromBob + `,"dataMessage":{"message":"let me in"}}`,
		`{"sourceUuid":"` + aliceID + `","dataMessage":{"attachments":[{"contentType":"application/pdf","id":"a","size":10}]}}`,
		`{"sourceUuid":"` + aliceID + `"}`,
	} {
		if r, ok := event(env); ok {
			t.Errorf("%s reached the core: %+v", env, r)
		}
	}
	if out := logs.String(); out != bobID+" ignored because not in SIGNAL_DM_USER_IDS\n" {
		t.Errorf("logged %q", out)
	}
}

// Before the bot's ID is known, every message waits in its slot, and Load
// decides it once the ID is known.
func TestEventsBeforeTheIDIsKnown(t *testing.T) {
	logs := captureLogs(t)
	c := newClient("x", map[string]bool{aliceID: true}, nil)
	c.names.refreshedAt = time.Now() // so loading names doesn't try the socket
	var rs []bot.Received
	for _, env := range []string{
		`{"sourceUuid":"` + aliceID + `","sourceName":"Alice","dataMessage":{"message":"hi"}}`,
		`{` + fromBob + `,"dataMessage":{"message":"the moon",` + inGroup + `}}`,
		`{` + fromBob + `,"dataMessage":{"message":"let me in"}}`,
	} {
		r, ok := c.event(parse(t, env))
		if !ok || r.Addressed || r.Load == nil {
			t.Fatalf("%s: %+v", env, r)
		}
		rs = append(rs, r)
	}
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := rs[0].Load(gone, nil); ok {
		t.Fatal("loaded without the ID")
	}
	connected(c)
	learn := func(name, id string) { t.Errorf("a DM showed %s", name) }
	ctx := context.Background()
	if m, ok := rs[0].Load(ctx, learn); !ok || !m.Addressed || m.Text != "hi" || m.Name != "Alice" {
		t.Errorf("DM loaded as %+v %v", m, ok)
	}
	if m, ok := rs[1].Load(ctx, learn); !ok || m.Addressed || m.Text != "the moon" {
		t.Errorf("chatter loaded as %+v %v", m, ok)
	}
	if _, ok := rs[2].Load(ctx, learn); ok || !strings.Contains(logs.String(), bobID+" ignored because not in SIGNAL_DM_USER_IDS") {
		t.Errorf("a stranger's DM loaded; logs %q", logs)
	}
}

// Loading an addressed message loads the names it needs, shows its sticker's
// label, and passes learn the names the model sees.
func TestLoad(t *testing.T) {
	captureLogs(t)
	f := newFake(t)
	f.set("listContacts", `[{"uuid":"`+botID+`","givenName":"Botty"},{"uuid":"`+aliceID+`","givenName":"Alice"}]`)
	f.set("listStickerPacks", `[{"packId":"p","title":"Pack","stickers":[{"id":2,"emoji":"🙂"}]}]`)
	c := newClient(f.sock, nil, nil)
	got, conn := run(t, c, f)
	receive(conn, `{`+fromBob+`,"dataMessage":{"message":"￼ ask ￼","sticker":{"packId":"p","stickerId":2},`+
		`"mentions":[{"uuid":"`+botID+`","start":0,"length":1},{"uuid":"`+aliceID+`","start":6,"length":1}],`+inGroup+`}}`)
	r := next(t, got)
	var learned []string
	m, ok := r.Load(context.Background(), func(name, id string) { learned = append(learned, name+" "+id) })
	if !ok || m.Text != "ask @Alice" || len(m.Media) != 1 || m.Media[0].Label != `: 🙂 from "Pack"` {
		t.Fatalf("loaded %+v %v", m, ok)
	}
	if !reflect.DeepEqual(learned, []string{"Bob " + bobID, "Alice " + aliceID}) {
		t.Fatalf("learned %q", learned)
	}
}

// A delivered message is checked as if its sender had just posted it: a
// DM's sender must still be allowed, by ACI or number, and a group's still
// a member. Its sender's name is filled in, the bot's @name is taken out,
// and a group's members come back with it. One delivered before the client
// connects waits, and loads the bot's name to take out. A drop is logged
// without content.
func TestDelivered(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	f.set("listContacts", `[{"uuid":"`+botID+`","givenName":"Botty"},{"uuid":"`+aliceID+`","number":"+1555","givenName":"Alice"},{"uuid":"`+bobID+`","givenName":"Bob"}]`)
	f.set("listGroups", `[{"id":"`+groupID+`","name":"Family","isMember":true,"members":[{"uuid":"`+aliceID+`"},{"uuid":"`+bobID+`"}]}]`)
	allowed := map[string]bool{"+1555": true}
	c := newClient(f.sock, allowed, nil)
	ctx := context.Background()
	type result struct {
		r  bot.Received
		ok bool
	}
	early := make(chan result, 1)
	go func() {
		r, ok := c.Delivered(ctx, feature.Message{Chat: feature.Chat{User: aliceID}, Sender: aliceID, Text: "@Botty up?"})
		early <- result{r, ok}
	}()
	run(t, c, f)
	select {
	case e := <-early:
		if !e.ok || !e.r.Mentioned || e.r.Text != "up?" || e.r.Name != "Alice" {
			t.Fatalf("delivered before connecting: %+v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the early delivery never returned")
	}

	r, ok := c.Delivered(ctx, feature.Message{Chat: feature.Chat{GroupID: groupID}, Sender: aliceID, Text: "@BOTTY what's new, @Bob?"})
	if want := []bot.Member{{Name: "Alice", ID: aliceID}, {Name: "Bob", ID: bobID}}; !ok || !r.Mentioned || r.Text != "what's new, @Bob?" || r.Name != "Alice" || !reflect.DeepEqual(r.Members, want) {
		t.Fatalf("group: %+v %v", r, ok)
	}
	if r, ok := c.Delivered(ctx, feature.Message{Chat: feature.Chat{User: aliceID}, Sender: aliceID, Name: "Alice S", Text: "water the plants"}); !ok || r.Mentioned || r.Name != "Alice S" || r.Members != nil {
		t.Fatalf("DM allowed by number: %+v %v", r, ok)
	}
	if _, ok := c.Delivered(ctx, feature.Message{Chat: feature.Chat{GroupID: groupID}, Sender: bobID, Text: "hello all"}); !ok {
		t.Fatal("a group member not allowed to DM was dropped")
	}
	for _, m := range []feature.Message{
		{Chat: feature.Chat{GroupID: "other"}, Sender: aliceID, Text: "secret words"},
		{Chat: feature.Chat{User: bobID}, Sender: bobID, Text: "secret words"},
	} {
		if _, ok := c.Delivered(ctx, m); ok {
			t.Errorf("delivered %+v", m)
		}
	}
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if _, ok := newClient("x", nil, nil).Delivered(gone, feature.Message{Text: "secret words"}); ok {
		t.Error("delivered while not connected")
	}
	out := logs.String()
	for _, want := range []string{"delivered message dropped: its sender is not in the group\n",
		"delivered message from " + bobID + " dropped because not in SIGNAL_DM_USER_IDS\n",
		"delivered message dropped: not connected to signal-cli after 10s\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("no %q in logs %q", want, out)
		}
	}
	if strings.Contains(out, "secret") || strings.Contains(out, "plants") {
		t.Errorf("text logged: %q", out)
	}
}

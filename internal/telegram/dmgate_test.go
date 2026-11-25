package telegram

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/allow"
)

const (
	fromB   = `"from":{"id":2222222,"first_name":"Bob"}`
	inBobDM = `"chat":{"id":2222222,"type":"private"}`
)

// set makes the fake answer method with result from now on.
func (f *fakeAPI) set(method, result string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.results[method] = result
}

// fail makes the fake's next call to method fail with resp.
func (f *fakeAPI) fail(method, resp string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.errs[method] = resp
}

// gated is a client with TELEGRAM_DM_USERS value, that has seen the
// test group, whose owner is Alice.
func gated(t *testing.T, value string) (*fakeAPI, *Client) {
	f := newFake(t)
	f.set("getChatAdministrators", `[{"status":"creator","user":{"id":1111111,"first_name":"Alice"}}]`)
	c := connected(f.client())
	c.dm = allow.Parse(value)
	c.event(parse(t, `{"update_id":1,"message":{"message_id":1,`+fromB+`,`+inGroup+`,"text":"hi"}}`))
	return f, c
}

// dm hands c a DM and, if it is Incoming, loads it: whether the reader
// kept it, and whether it was answered.
func dm(t *testing.T, c *Client, from, chat string) (kept, answered bool) {
	r, ok := c.event(parse(t, `{"update_id":2,"message":{"message_id":10,`+from+`,`+chat+`,"text":"hi"}}`))
	if !ok {
		return false, false
	}
	in, isIn := r.(bot.Incoming)
	if !isIn {
		t.Fatalf("a DM as %T", r)
	}
	_, answered = in.Load(context.Background(), func(string, string) {})
	return true, answered
}

// By default an admin of a group the bot has seen may DM it, asked in the
// message's slot and kept 10 minutes; anyone else is refused, logged by ID
// only and once, then on the reader with no lookup. A refused edit forgets the
// original, and a refused reaction or press is dropped.
func TestDMAdmins(t *testing.T) {
	logs := captureLogs(t)
	f, c := gated(t, "")
	c.Watch(bot.Watch{Events: true})
	if kept, answered := dm(t, c, fromA, inDM); !kept || !answered {
		t.Fatal("an admin's DM refused")
	}
	if kept, answered := dm(t, c, fromB, inBobDM); !kept || answered {
		t.Fatalf("a stranger's DM: kept %v, answered %v", kept, answered)
	}
	if kept, _ := dm(t, c, fromB, inBobDM); kept {
		t.Fatal("a refused stranger's DM handed on again")
	}
	if kept, answered := dm(t, c, fromA, inDM); !kept || !answered {
		t.Fatal("an admin's second DM refused")
	}
	if n := len(f.callsTo("getChatAdministrators")); n != 1 {
		t.Fatalf("%d admin lookups, want 1, the group's kept", n)
	}
	out := logs.String()
	if strings.Count(out, "Telegram: a DM from 2222222 ignored: TELEGRAM_DM_USERS doesn't allow it; add the ID to TELEGRAM_DM_USERS to allow DMs from them\n") != 1 || strings.Count(out, "a DM from") != 1 || strings.Contains(out, "1111111") || strings.Contains(out, "Bob") || strings.Contains(out, "Alice") {
		t.Fatalf("logs %q", out)
	}
	r, _ := c.event(parse(t, `{"update_id":3,"edited_message":{"message_id":7,`+fromB+`,`+inBobDM+`,"date":100,"edit_date":300,"text":"x"}}`))
	if d, ok := r.(bot.Deleted); !ok || d.Sender != "2222222" || d.Sent != 7 || d.At != 300 {
		t.Fatalf("a refused edit %+v", r)
	}
	react := func(from, chat string) bool {
		_, ok := c.event(parse(t, `{"update_id":4,"message_reaction":{`+chat+`,"message_id":500,`+strings.Replace(from, `"from"`, `"user"`, 1)+`,"old_reaction":[],"new_reaction":[{"type":"emoji","emoji":"👍"}]}}`))
		return ok
	}
	if !react(fromA, inDM) || react(fromB, inBobDM) {
		t.Fatal("reactions")
	}
	press := func(from, chat string) bool {
		_, ok := c.event(parse(t, `{"update_id":5,"callback_query":{"id":"q1",`+from+`,"message":{"message_id":500,`+chat+`,"date":1},"data":"rain 7d"}}`))
		return ok
	}
	if !press(fromA, inDM) || press(fromB, inBobDM) {
		t.Fatal("presses")
	}
	waitCalls(t, f, "answerCallbackQuery", 1) // the refused press stops spinning
}

// A DM's reaction or press is decided on the reader: with no verdict kept,
// only a listed ID's is handed on, an admin's not yet.
func TestDMReaderNeedsAVerdict(t *testing.T) {
	_, c := gated(t, "")
	if _, ok := c.event(parse(t, `{"update_id":5,"callback_query":{"id":"q1",`+fromA+`,"message":{"message_id":500,`+inDM+`,"date":1},"data":"x"}}`)); ok {
		t.Fatal("a press with no verdict kept")
	}
	c.dm = allow.Parse("admins telegram:1111111")
	if _, ok := c.event(parse(t, `{"update_id":5,"callback_query":{"id":"q2",`+fromA+`,"message":{"message_id":500,`+inDM+`,"date":1},"data":"x"}}`)); !ok {
		t.Fatal("a listed ID's press")
	}
}

// A listed ID, with "telegram:" or without, or "all", is answered with no
// lookup; "none" and "mods" (Telegram has none) refuse everyone on the
// reader.
func TestDMListed(t *testing.T) {
	captureLogs(t)
	for _, tc := range []struct {
		value      string
		alice, bob bool
	}{
		{"2222222", false, true},
		{"telegram:2222222", false, true},
		{"all", true, true},
		{"none", false, false},
		{"mods", false, false},
		{"none, 1111111", true, false},
	} {
		f, c := gated(t, tc.value)
		if kept, answered := dm(t, c, fromA, inDM); answered != tc.alice || kept != tc.alice {
			t.Errorf("%q: Alice kept %v, answered %v", tc.value, kept, answered)
		}
		if kept, answered := dm(t, c, fromB, inBobDM); answered != tc.bob || kept != tc.bob {
			t.Errorf("%q: Bob kept %v, answered %v", tc.value, kept, answered)
		}
		if n := len(f.callsTo("getChatAdministrators")); n != 0 {
			t.Errorf("%q: %d admin lookups", tc.value, n)
		}
	}
}

// A failed admin lookup refuses, warning without naming anyone, and isn't
// kept: the next DM asks again. A group the bot can't ask about any more
// counts as no.
func TestDMFailsClosed(t *testing.T) {
	logs := captureLogs(t)
	f, c := gated(t, "")
	f.fail("getChatAdministrators", `{"ok":false,"error_code":500,"description":"Internal Server Error"}`)
	if _, answered := dm(t, c, fromA, inDM); answered {
		t.Fatal("answered when the lookup failed")
	}
	for _, l := range strings.Split(logs.String(), "\n") {
		if strings.Contains(l, "admin failed") && (strings.Contains(l, "1111111") || strings.Contains(l, "Alice")) {
			t.Fatalf("the warning names someone: %q", l)
		}
	}
	if !strings.Contains(logs.String(), "checking whether its user is an admin failed") {
		t.Fatalf("logs %q", logs)
	}
	if _, answered := dm(t, c, fromA, inDM); !answered {
		t.Fatal("a failure was kept")
	}

	f, c = gated(t, "")
	f.fail("getChatAdministrators", `{"ok":false,"error_code":403,"description":"Forbidden: bot was kicked"}`)
	if _, answered := dm(t, c, fromA, inDM); answered {
		t.Fatal("answered by a group the bot can't ask about")
	}
	if kept, _ := dm(t, c, fromA, inDM); kept {
		t.Fatal("a no from a group gone wasn't kept")
	}
}

// Who is an admin is asked again when an admin's status changes, or the bot
// joins or leaves a group; a group first seen makes a refusal ask again.
func TestDMForgets(t *testing.T) {
	captureLogs(t)
	f, c := gated(t, "")
	dm(t, c, fromA, inDM)
	f.set("getChatAdministrators", `[]`)
	c.event(parse(t, `{"update_id":6,"chat_member":{`+inGroup+`,`+fromB+`,"old_chat_member":{"status":"creator"},"new_chat_member":{"status":"member","user":{"id":1111111,"first_name":"Alice"}}}}`))
	if _, answered := dm(t, c, fromA, inDM); answered {
		t.Fatal("answered once no longer an admin")
	}
	f.set("getChatAdministrators", `[{"status":"administrator","user":{"id":1111111,"first_name":"Alice"}}]`)
	c.event(parse(t, `{"update_id":7,"message":{"message_id":2,`+fromB+`,"chat":{"id":-1009999999,"type":"supergroup","title":"Another"},"text":"hi"}}`))
	if _, answered := dm(t, c, fromA, inDM); !answered {
		t.Fatal("refused after a new group")
	}
	f.set("getChatAdministrators", `[]`)
	for _, g := range []string{inGroup, `"chat":{"id":-1009999999,"type":"supergroup","title":"Another"}`} {
		c.event(parse(t, `{"update_id":8,"my_chat_member":{`+g+`,`+fromB+`,"old_chat_member":{"status":"member"},"new_chat_member":{"status":"kicked","user":{"id":777000}}}}`))
	}
	if _, answered := dm(t, c, fromA, inDM); answered {
		t.Fatal("answered after the bot left the groups")
	}
}

// A message delivered to a DM goes only while its sender may DM the bot,
// and the drop names no one.
func TestDMDelivered(t *testing.T) {
	logs := captureLogs(t)
	_, c := gated(t, "")
	ctx := context.Background()
	if _, ok := c.Delivered(ctx, feature.Message{Chat: feature.Chat{User: "1111111"}, Sender: "1111111", Text: "x"}); !ok {
		t.Fatal("an admin's dropped")
	}
	if _, ok := c.Delivered(ctx, feature.Message{Chat: feature.Chat{User: "2222222"}, Sender: "2222222", Text: "x"}); ok {
		t.Fatal("a stranger's delivered")
	}
	c.event(parse(t, `{"update_id":8,"my_chat_member":{`+inGroup+`,`+fromB+`,"old_chat_member":{"status":"member"},"new_chat_member":{"status":"kicked","user":{"id":777000}}}}`))
	if _, ok := c.Delivered(ctx, feature.Message{Chat: feature.Chat{User: "1111111"}, Sender: "1111111", Text: "x"}); ok {
		t.Fatal("delivered once its sender may no longer DM the bot")
	}
	if out := logs.String(); strings.Count(out, "delivered message dropped: its sender may no longer DM the bot\n") != 2 || strings.Contains(out, "1111111") || strings.Contains(out, "2222222") {
		t.Fatalf("logs %q", out)
	}
}

// Groups marks a group whose owner or administrator the user is; Telegram
// has no mods.
func TestGroupsAdmin(t *testing.T) {
	f, c := gated(t, "")
	for user, want := range map[string]bool{"1111111": true, "2222222": false} {
		gs, _ := c.Groups(context.Background(), user)
		if !reflect.DeepEqual(gs, []feature.Group{{ID: groupS, Name: "Test group", Admin: want}}) {
			t.Errorf("%s: %+v", user, gs)
		}
	}
	if n := len(f.callsTo("getChatAdministrators")); n != 1 {
		t.Errorf("%d admin lookups, want 1", n)
	}
}

// The startup log warns when no one may DM the bot, and is otherwise
// silent about DMs.
func TestDMStartupLog(t *testing.T) {
	for value, want := range map[string]string{
		"":              "",
		"all":           "",
		"admins 1 2":    "",
		"none":          "Telegram: DMs are off: TELEGRAM_DM_USERS is none; the bot answers only in groups\n",
		"mods":          "Telegram: DMs are off: TELEGRAM_DM_USERS allows only mods, and Telegram has none; the bot answers only in groups\n",
		"telegram:1111": "",
	} {
		logs := captureLogs(t)
		t.Setenv("TELEGRAM_DM_USERS", value)
		New(nil)
		if logs.String() != want {
			t.Errorf("%q: %q", value, logs)
		}
	}
}

// A lookup under way when Alice stops being an admin keeps neither her
// group's administrators nor her verdict, as both may be from before.
func TestDMForgetsDuringLookup(t *testing.T) {
	captureLogs(t)
	f, c := gated(t, "")
	asked, release := make(chan struct{}), make(chan struct{})
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := httptest.NewRecorder()
		f.serve(rec, r)
		if strings.HasSuffix(r.URL.Path, "/getChatAdministrators") {
			close(asked) // answered, but held till she is demoted
			<-release
		}
		w.Write(rec.Body.Bytes())
	}))
	t.Cleanup(slow.Close)
	c.base = slow.URL
	done := make(chan bool)
	go func() {
		_, answered := dm(t, c, fromA, inDM)
		done <- answered
	}()
	<-asked
	f.set("getChatAdministrators", `[]`)
	c.event(parse(t, `{"update_id":6,"chat_member":{`+inGroup+`,`+fromB+`,"old_chat_member":{"status":"creator"},"new_chat_member":{"status":"member","user":{"id":1111111,"first_name":"Alice"}}}}`))
	close(release)
	<-done
	c.base = f.srv.URL
	if _, answered := dm(t, c, fromA, inDM); answered {
		t.Fatal("answered by what a lookup begun before her demotion said")
	}
}

// The bot may post on its own in a DM only while TELEGRAM_DM_USERS allows
// its user: listed, or "all", with no lookup; else an admin of a group it
// has seen, asked and kept. A failed lookup refuses, with a warning naming
// no one.
func TestReachableDM(t *testing.T) {
	logs := captureLogs(t)
	ctx := context.Background()
	for _, v := range []string{"2222222", "telegram:2222222", "all"} {
		f, c := gated(t, v)
		if !c.Reachable(ctx, feature.Chat{User: "2222222"}) {
			t.Fatalf("%q: Bob's DM", v)
		}
		if n := len(f.callsTo("getChatAdministrators")); n != 0 {
			t.Fatalf("%q: %d admin lookups", v, n)
		}
	}
	if _, c := gated(t, "none"); c.Reachable(ctx, feature.Chat{User: "1111111"}) {
		t.Fatal("none: Alice's DM")
	}

	f, c := gated(t, "")
	if !c.Reachable(ctx, feature.Chat{User: "1111111"}) || c.Reachable(ctx, feature.Chat{User: "2222222"}) {
		t.Fatal("by default, the owner's DM only")
	}
	f.set("getChatAdministrators", `[]`)
	c.event(parse(t, `{"update_id":6,"chat_member":{`+inGroup+`,`+fromB+`,"old_chat_member":{"status":"creator"},"new_chat_member":{"status":"member","user":{"id":1111111,"first_name":"Alice"}}}}`))
	if c.Reachable(ctx, feature.Chat{User: "1111111"}) {
		t.Fatal("Alice's DM once she is no longer the owner")
	}

	f, c = gated(t, "")
	f.fail("getChatAdministrators", `{"ok":false,"error_code":500,"description":"Internal Server Error"}`)
	if c.Reachable(ctx, feature.Chat{User: "1111111"}) {
		t.Fatal("reachable when the lookup failed")
	}
	if l := logs.String(); !strings.Contains(l, "checking whether its user is an admin failed") || strings.Contains(l, "1111111") || strings.Contains(l, "Alice") {
		t.Fatalf("logs %q", l)
	}
}

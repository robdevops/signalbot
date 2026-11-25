package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/allow"
)

// adminAlice is users.info's answer for Alice as a workspace admin.
const adminAlice = `"user":{"name":"alice","is_admin":true,"profile":{"display_name":"Alice"}}`

// dmFrom is a DM event from user, numbered n, with their name, so only
// the gate asks users.info.
func dmFrom(user string, n int) envelope {
	name := map[string]string{alice: "Alice", bob: "Bob"}[user]
	return ev(fmt.Sprintf(`{"type":"message","channel":"D0%sDM","channel_type":"im","user":%q,"text":"hi","ts":"1700000000.%06d","user_profile":{"display_name":%q}}`, user[1:5], user, n, name))
}

// load is rs's one Incoming loaded, and whether Load kept it; "none" if
// the reader dropped it.
func load(t *testing.T, rs []bot.Received) string {
	t.Helper()
	if len(rs) == 0 {
		return "none"
	}
	in, ok := rs[0].(bot.Incoming)
	if !ok {
		t.Fatalf("got %T", rs[0])
	}
	if m, ok := in.Load(context.Background(), func(string, string) {}); ok {
		return m.Text
	}
	return "refused"
}

// By default a workspace admin's DM is answered, asking users.info once
// in its slot, and a stranger's isn't, logged by their ID alone; once
// known, the stranger is refused on the reader, without a slot.
func TestDMDefaultAdmins(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	f.infos = map[string]string{alice: adminAlice}
	c := connected(f.client(t))
	c.dm = allow.Parse("")
	if got := load(t, c.events(dmFrom(alice, 1))); got != "hi" {
		t.Fatalf("Alice: %s", got)
	}
	if got := load(t, c.events(dmFrom(alice, 2))); got != "hi" {
		t.Fatalf("Alice again: %s", got)
	}
	if n := len(f.callsTo("users.info")); n != 1 {
		t.Fatalf("users.info asked %d times", n)
	}
	if got := load(t, c.events(dmFrom(bob, 3))); got != "refused" {
		t.Fatalf("Bob: %s", got)
	}
	if got := load(t, c.events(dmFrom(bob, 4))); got != "none" {
		t.Fatalf("Bob again: %s", got)
	}
	want := "slack:" + bob + " ignored because not allowed by SLACK_DM_USER_IDS (add this ID to allow DMs)\n"
	if l := logs.String(); l != want+want {
		t.Fatalf("log %q", l)
	}
	// In a channel anyone is answered, with no lookup.
	n := len(f.callsTo("users.info"))
	if got := load(t, c.events(ev(`{"type":"message","channel":"C0CHANNEL1","channel_type":"channel","user":"UBOB00001","text":"<@UBOT00001> hi","ts":"1700000000.000005"}`))); got != "hi" {
		t.Fatalf("Bob in a channel: %s", got)
	}
	if len(f.callsTo("users.info")) != n {
		t.Fatal("users.info asked for a channel message")
	}
}

// A listed ID, with "slack:" or without, is answered with no lookup; "none"
// refuses everyone on the reader, an admin too; "mods" alone lets no one
// by role, as Slack has none for a bot.
func TestDMListed(t *testing.T) {
	captureLogs(t)
	for _, tc := range []struct {
		value      string
		alice, bob string
	}{
		{bob, "none", "hi"},
		{"slack:" + bob, "none", "hi"},
		{"none", "none", "none"},
		{"mods", "none", "none"},
		{"all", "hi", "hi"},
		{"admins, " + bob, "hi", "hi"},
	} {
		f := newFake(t)
		f.infos = map[string]string{alice: adminAlice}
		c := connected(f.client(t))
		c.dm = allow.Parse(tc.value)
		a, b := load(t, c.events(dmFrom(alice, 1))), load(t, c.events(dmFrom(bob, 2)))
		if a != tc.alice || b != tc.bob {
			t.Errorf("%q: Alice %s, Bob %s", tc.value, a, b)
		}
		if n := len(f.callsTo("users.info")); n != 0 && !strings.Contains(tc.value, "admins") {
			t.Errorf("%q: users.info asked %d times", tc.value, n)
		}
	}
}

// A failed lookup refuses, with a warning naming no one, and is not kept,
// so the next DM asks again.
func TestDMLookupFails(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	f.infos = map[string]string{alice: adminAlice}
	c := connected(f.client(t))
	c.dm = allow.Parse("")
	f.fail("users.info", "internal_error")
	if got := load(t, c.events(dmFrom(alice, 1))); got != "refused" {
		t.Fatalf("got %s", got)
	}
	if l := logs.String(); !strings.Contains(l, "slack users.info failed, so a DM's user is taken for no admin: slack: users.info: internal_error\n") || strings.Count(l, alice) != 1 {
		t.Fatalf("log %q", l)
	}
	if got := load(t, c.events(dmFrom(alice, 2))); got != "hi" {
		t.Fatalf("again: %s", got)
	}
}

// An admin of another workspace, in a channel shared with the bot's, is no
// admin of the bot's; one of the same Enterprise Grid org is.
func TestDMOtherWorkspace(t *testing.T) {
	captureLogs(t)
	f := newFake(t)
	f.infos = map[string]string{
		alice: `"user":{"name":"alice","team_id":"T2","is_owner":true,"enterprise_user":{"enterprise_id":"E1"}}`,
		bob:   `"user":{"name":"bob","team_id":"T3","is_admin":true}`,
	}
	c := connected(f.client(t))
	c.team.Store([2]string{"T1", "E1"})
	c.dm = allow.Parse("")
	if a, b := load(t, c.events(dmFrom(alice, 1))), load(t, c.events(dmFrom(bob, 2))); a != "hi" || b != "refused" {
		t.Fatalf("Alice %s, Bob %s", a, b)
	}
	f.infos[bob] = `"user":{"name":"bob","team_id":"T1","is_stranger":true,"is_primary_owner":true}`
	c.verdicts = verdicts{}
	if b := load(t, c.events(dmFrom(bob, 3))); b != "refused" {
		t.Fatalf("a stranger: %s", b)
	}
}

// A slash command in a DM is gated as a DM message is; in a channel it
// isn't. The envelope was acknowledged either way.
func TestDMSlashCommand(t *testing.T) {
	captureLogs(t)
	f := newFake(t)
	c := connected(f.client(t))
	c.dm = allow.Parse("")
	dm := envelope{Type: "slash_commands", EnvelopeID: "e1", Payload: json.RawMessage(`{"command":"/hello","channel_id":"D0BOBDM01","user_id":"UBOB00001"}`)}
	if got := load(t, c.events(dm)); got != "refused" {
		t.Fatalf("first: %s", got)
	}
	if got := load(t, c.events(dm)); got != "none" {
		t.Fatalf("again: %s", got)
	}
	in := envelope{Type: "slash_commands", EnvelopeID: "e2", Payload: json.RawMessage(`{"command":"/hello","channel_id":"C0CHANNEL1","user_id":"UBOB00001"}`)}
	if got := load(t, c.events(in)); got != "/hello" {
		t.Fatalf("channel: %s", got)
	}
}

// A reaction or a press in a DM, decided on the reader, counts only a
// listed user or one with a fresh admin verdict; in a channel, anyone's.
func TestDMReactionsAndPresses(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	f.infos = map[string]string{alice: adminAlice}
	c := connected(f.client(t))
	c.dm = allow.Parse("")
	c.watch.Store(true)
	c.mu.Lock()
	c.noteIM(alice, "D0ALICEDM1")
	c.mu.Unlock()
	react := func(ch string) []bot.Received {
		return c.events(ev(`{"type":"reaction_added","user":"UALICE001","reaction":"thumbsup","item_user":"UBOT00001","item":{"type":"message","channel":"` + ch + `","ts":"1700000100.000001"}}`))
	}
	press := func(ch string) []bot.Received {
		return c.events(envelope{Type: "interactive", Payload: json.RawMessage(`{"type":"block_actions","user":{"id":"UALICE001","name":"alice"},"channel":{"id":"` + ch + `"},
			"container":{"message_ts":"1700000100.000001"},"actions":[{"value":"x"}]}`)})
	}
	if len(react("D0ALICEDM1")) != 0 || len(press("D0ALICEDM1")) != 0 {
		t.Fatal("handed on without a verdict")
	}
	if len(react(channel)) != 1 || len(press(channel)) != 1 {
		t.Fatal("a channel's dropped")
	}
	if len(f.callsTo("users.info")) != 0 {
		t.Fatal("asked users.info on the reader")
	}
	if got := load(t, c.events(dmFrom(alice, 1))); got != "hi" {
		t.Fatalf("DM: %s", got)
	}
	if len(react("D0ALICEDM1")) != 1 || len(press("D0ALICEDM1")) != 1 {
		t.Fatal("dropped with a fresh verdict")
	}
	if strings.Contains(logs.String(), "Alice") {
		t.Fatalf("a name logged: %q", logs.String())
	}
}

// Delivered to a DM goes while its user may DM the bot, asked afresh, and
// is dropped, naming no one, once they may not or Slack can't say;
// Reachable says the same from the verdict kept.
func TestDeliveredToDM(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	f.infos = map[string]string{alice: adminAlice}
	c := connected(f.client(t))
	c.dm = allow.Parse("")
	ctx := context.Background()
	msg := feature.Message{Chat: feature.Chat{User: "slack:" + alice}, Sender: "slack:" + alice, Text: "hi"}
	if _, ok := c.Delivered(ctx, msg); !ok {
		t.Fatal("dropped for an admin")
	}
	if !c.Reachable(ctx, msg.Chat) || c.Reachable(ctx, feature.Chat{User: "slack:" + bob}) {
		t.Fatal("Reachable")
	}
	f.mu.Lock()
	f.infos[alice] = `"user":{"name":"alice"}`
	f.mu.Unlock()
	if _, ok := c.Delivered(ctx, msg); ok {
		t.Fatal("delivered once Alice is no admin")
	}
	f.fail("users.info", "internal_error")
	if _, ok := c.Delivered(ctx, msg); ok {
		t.Fatal("delivered when Slack couldn't say")
	}
	l := logs.String()
	if strings.Count(l, "delivered message dropped: its sender may no longer DM the bot (SLACK_DM_USER_IDS)\n") != 2 || strings.Contains(l, alice) || strings.Contains(l, "Alice") {
		t.Fatalf("log %q", l)
	}
	c.dm = allow.Parse(alice)
	if _, ok := c.Delivered(ctx, msg); !ok {
		t.Fatal("dropped for a listed user")
	}
}

// Groups marks every channel Admin for a workspace admin and none for
// anyone else, Mod never; a failed lookup marks none, naming no one.
func TestGroupsAdmin(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	f.infos = map[string]string{alice: adminAlice}
	f.convs = map[string]string{" ": `"channels":[{"id":"C0CHANNEL1","name":"team"}]`, bob + " ": `"channels":[{"id":"C0CHANNEL1","name":"team"}]`}
	c := connected(f.client(t))
	for _, tc := range []struct {
		user  string
		admin bool
	}{{alice, true}, {bob, false}} {
		gs, err := c.Groups(context.Background(), "slack:"+tc.user)
		if err != nil || !reflect.DeepEqual(gs, []feature.Group{{ID: "slack:" + channel, Name: "#team", Admin: tc.admin}}) {
			t.Fatalf("%s: %+v %v", tc.user, gs, err)
		}
	}
	c.verdicts = verdicts{}
	f.fail("users.info", "internal_error")
	gs, _ := c.Groups(context.Background(), "slack:"+alice)
	if len(gs) != 1 || gs[0].Admin {
		t.Fatalf("groups %+v", gs)
	}
	if l := logs.String(); !strings.Contains(l, "taken for no admin of their channels") || strings.Contains(l, alice) {
		t.Fatalf("log %q", l)
	}
}

// The verdicts kept are bounded, stale ones dropped first, and go stale
// after verdictAge.
func TestVerdictsBounded(t *testing.T) {
	var v verdicts
	now := time.Now()
	for i := range verdictMax {
		v.put(fmt.Sprint(i), true, now)
	}
	v.m["0"] = verdict{true, time.Now().Add(-verdictAge - time.Second)}
	if _, ok := v.get("0"); ok {
		t.Fatal("a stale verdict believed")
	}
	v.put("new", false, now)
	if len(v.m) != verdictMax {
		t.Fatalf("%d kept", len(v.m))
	}
	v.put("newer", true, now)
	if admin, ok := v.get("newer"); !ok || !admin || len(v.m) != 1 {
		t.Fatalf("%d kept", len(v.m))
	}
}

// The startup log says who may DM the bot, or that no one may.
func TestDMStartupLog(t *testing.T) {
	logs := captureLogs(t)
	for _, v := range []string{"", "all", "none", "admins " + alice} {
		t.Setenv("SLACK_DM_USER_IDS", v)
		New(nil)
	}
	want := "Slack DMs: allowed from admins and mods (SLACK_DM_USER_IDS)\n" +
		"Slack DMs: allowed from anyone (SLACK_DM_USER_IDS)\n" +
		"Slack DMs are off: SLACK_DM_USER_IDS is none; the bot answers only in channels\n" +
		"Slack DMs: allowed from admins and 1 ID (SLACK_DM_USER_IDS)\n"
	if l := logs.String(); l != want {
		t.Fatalf("log %q", l)
	}
}

// A lookup asked before another, answering after it, doesn't replace the
// later one's verdict: a demoted admin's slow old answer can't let them
// back in for another verdictAge.
func TestVerdictOlderAnswerLoses(t *testing.T) {
	var v verdicts
	now := time.Now()
	v.put(alice, false, now)
	v.put(alice, true, now.Add(-time.Second))
	if admin, ok := v.get(alice); !ok || admin {
		t.Fatalf("admin %v, ok %v", admin, ok)
	}
	v.put(alice, true, now.Add(time.Second))
	if admin, _ := v.get(alice); !admin {
		t.Fatal("a newer answer not kept")
	}
}

// Before auth.test has said which workspace is the bot's, no one is its
// admin, and nothing is kept; Groups waits for it, and without it marks
// no channel Admin.
func TestAdminBeforeConnected(t *testing.T) {
	captureLogs(t)
	f := newFake(t)
	f.infos = map[string]string{alice: adminAlice}
	c := f.client(t)
	if u, err := c.userInfo(context.Background(), alice); err != nil || u.admin {
		t.Fatalf("%+v %v", u, err)
	}
	if _, ok := c.verdicts.get(alice); ok {
		t.Fatal("a verdict kept before auth.test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if admin, err := c.isAdmin(ctx, alice); err == nil || admin {
		t.Fatalf("admin %v, err %v", admin, err)
	}
	connected(c)
	if admin, err := c.isAdmin(context.Background(), alice); err != nil || !admin {
		t.Fatalf("connected: admin %v, err %v", admin, err)
	}
}

// An edit of a DM is gated as the DM is: a stranger's is dropped, an
// admin's goes on.
func TestDMEditGated(t *testing.T) {
	captureLogs(t)
	f := newFake(t)
	f.infos = map[string]string{alice: adminAlice}
	c := connected(f.client(t))
	c.dm = allow.Parse("")
	edit := func(user string) envelope {
		return ev(fmt.Sprintf(`{"type":"message","subtype":"message_changed","channel":"D0%sDM","channel_type":"im","ts":"1700000001.000001",
			"message":{"user":%q,"text":"hi again","ts":"1700000000.000001"},"previous_message":{"user":%q,"text":"hi","ts":"1700000000.000001"}}`, user[1:5], user, user))
	}
	if got := load(t, c.events(edit(bob))); got != "refused" {
		t.Fatalf("Bob: %s", got)
	}
	if got := load(t, c.events(edit(bob))); got != "none" {
		t.Fatalf("Bob again: %s", got)
	}
	if got := load(t, c.events(edit(alice))); got != "hi again" {
		t.Fatalf("Alice: %s", got)
	}
}

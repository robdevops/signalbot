package signal

import (
	"context"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/allow"
)

// otherID is a group the bot is in but SIGNAL_GROUP_IDS doesn't list.
const otherID = "b3RoZXI="

// twoGroups is listGroups with the bot in the listed group, whose admin is
// Bob, and in another, whose admin is Alice; both have Alice and Bob.
const twoGroups = `[{"id":"` + groupID + `","name":"Family","isMember":true,"members":[{"uuid":"` + aliceID + `"},{"uuid":"` + bobID + `"}],"admins":[{"uuid":"` + bobID + `"}]},` +
	`{"id":"` + otherID + `","name":"Other","isMember":true,"members":[{"uuid":"` + aliceID + `"},{"uuid":"` + bobID + `"}],"admins":[{"uuid":"` + aliceID + `"}]}]`

func TestParseGroups(t *testing.T) {
	for in, want := range map[string]groupSet{
		"":                       {all: true},
		"all":                    {all: true},
		"a=, ALL":                {all: true},
		"none":                   {ids: map[string]bool{}},
		"a+/=,b=;\n c= none\tb=": {ids: map[string]bool{"a+/=": true, "b=": true, "c=": true}},
		"a=,b=  # all of us, x=": {ids: map[string]bool{"a=": true, "b=": true}},
		"# all":                  {all: true},
	} {
		got := parseGroups(in)
		if got.all != want.all || !maps.Equal(got.ids, want.ids) || got.all && got.ids != nil {
			t.Errorf("%q: %+v, want %+v", in, got, want)
		}
	}
	s := parseGroups("a=")
	if !s.has("a=") || s.has("A=") || s.has("b=") || parseGroups("none").has("a=") || !parseGroups("").has("b=") {
		t.Error("has")
	}
}

// New says at startup which groups the bot answers in when they are
// listed, or none; for all, it warns once connected instead.
func TestNewGroups(t *testing.T) {
	logs := captureLogs(t)
	t.Setenv("SIGNAL_DM_USERS", "none")
	t.Setenv("SIGNAL_GROUP_IDS", "")
	if c := New(nil); !c.groupIDs.all || !c.suggest.Load() {
		t.Errorf("unset: %+v", c.groupIDs)
	}
	t.Setenv("SIGNAL_GROUP_IDS", groupID+","+otherID)
	if c := New(nil); c.groupIDs.all || len(c.groupIDs.ids) != 2 || c.suggest.Load() {
		t.Errorf("listed: %+v", c.groupIDs)
	}
	t.Setenv("SIGNAL_GROUP_IDS", "None")
	if c := New(nil); c.groupIDs.has(groupID) {
		t.Errorf("none: %+v", c.groupIDs)
	}
	out := logs.String()
	if !strings.Contains(out, "Signal groups: answering in 2 listed in SIGNAL_GROUP_IDS, and ignoring any other\n") ||
		!strings.Contains(out, "Signal groups are off: SIGNAL_GROUP_IDS is none; the bot answers only in DMs\n") ||
		strings.Contains(out, groupID) || strings.Contains(out, "any group") {
		t.Errorf("logs %q", out)
	}
}

// Nothing from a group SIGNAL_GROUP_IDS doesn't list reaches the core, nor
// is kept: messages, chatter, reactions, updates, edits and deletes, the
// account's own synced ones too. The group is logged once, by ID only.
func TestUnlistedGroupIgnored(t *testing.T) {
	logs := captureLogs(t)
	c := newClient("x", only(aliceID), nil)
	c.closed.Store(true) // so a group change schedules no contact load
	c.groupIDs = parseGroups(groupID)
	c.members.on.Store(true)
	connected(c)
	event := func(env string) (bot.Received, bool) { return c.event(parse(t, env)) }
	other := `"groupInfo":{"groupId":"` + otherID + `"}`

	for _, env := range []string{
		`{` + fromBob + `,"dataMessage":{"message":"￼ hi",` + atBot + `,` + other + `}}`,
		`{` + fromBob + `,"dataMessage":{"message":"/help",` + other + `}}`,
		`{` + fromBob + `,"timestamp":5,"dataMessage":{"message":"the moon",` + other + `,"attachments":[{"id":"a1","contentType":"image/png"}]}}`,
		`{` + fromBob + `,"dataMessage":{` + other + `,"reaction":{"emoji":"👍","targetAuthorUuid":"` + botID + `","targetSentTimestamp":5}}}`,
		`{` + fromBob + `,"dataMessage":{"groupInfo":{"groupId":"` + otherID + `","type":"UPDATE"}}}`,
		`{` + fromBob + `,"timestamp":9,"editMessage":{"targetSentTimestamp":5,"dataMessage":{"message":"x",` + other + `}}}`,
		`{` + fromBob + `,"dataMessage":{` + other + `,"remoteDelete":{"targetTimestamp":5}}}`,
		`{` + fromBob + `,"dataMessage":{` + other + `,"adminDelete":{"targetAuthorUuid":"` + aliceID + `","targetSentTimestamp":5}}}`,
		`{"sourceUuid":"` + botID + `","syncMessage":{"sentMessage":{` + other + `,"remoteDelete":{"targetTimestamp":5}}}}`,
	} {
		if r, ok := event(env); ok {
			t.Errorf("%s reached the core: %+v", env, r)
		}
	}
	if _, ok := c.recent.get(otherID, bobID, 5); ok {
		t.Error("an unlisted group's attachments kept")
	}
	if len(c.members.by) != 0 {
		t.Errorf("an unlisted group's update kept: %v", c.members.by)
	}
	if r, ok := event(`{` + fromBob + `,"dataMessage":{"message":"the moon",` + inGroup + `}}`); !ok {
		t.Error("a listed group's chatter dropped")
	} else if ch, isC := r.(bot.Chatter); !isC || ch.Message.Chat.GroupID != groupID {
		t.Errorf("chatter %+v", r)
	}
	if r, ok := event(`{` + fromBob + `,"dataMessage":{"message":"￼ hi",` + atBot + `,` + inGroup + `}}`); !ok {
		t.Error("a listed group's mention dropped")
	} else if _, isI := r.(bot.Incoming); !isI {
		t.Errorf("mention %+v", r)
	}
	if out := logs.String(); out != "a message in group "+otherID+" ignored: not in SIGNAL_GROUP_IDS\n" {
		t.Errorf("logged %q", out)
	}
}

// The ignored groups logged are bounded.
func TestIgnoredBounded(t *testing.T) {
	logs := captureLogs(t)
	c := newClient("x", allow.Policy{}, nil)
	c.groupIDs = parseGroups("none")
	for i := range ignoredMax + 5 {
		if !c.unlisted(strings.Repeat("g", i+1)) {
			t.Fatal("listed")
		}
	}
	if n := strings.Count(logs.String(), "\n"); n != ignoredMax || len(c.ignored.seen) != ignoredMax {
		t.Fatalf("%d lines, %d held", n, len(c.ignored.seen))
	}
	if c.unlisted("") {
		t.Fatal("a DM unlisted")
	}
}

// Groups, Delivered and Reachable know only listed groups, and only their
// admins may DM the bot as admins.
func TestListedGroupsOnly(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	f.set("listGroups", twoGroups)
	c := newClient(f.sock, allow.Parse(""), nil)
	c.groupIDs = parseGroups(groupID)
	c.names.refreshedAt = time.Now() // so loading names doesn't count
	run(t, c, f)
	ctx := context.Background()

	for _, user := range []string{aliceID, bobID} {
		want := feature.Group{ID: groupID, Name: "Family", Admin: user == bobID}
		if gs, err := c.Groups(ctx, user); err != nil || len(gs) != 1 || gs[0] != want {
			t.Fatalf("groups shared with %s: %+v %v", user, gs, err)
		}
	}
	if !c.Reachable(ctx, feature.Chat{GroupID: groupID}) || c.Reachable(ctx, feature.Chat{GroupID: otherID}) {
		t.Fatal("Reachable")
	}
	in := func(g string) feature.Message {
		return feature.Message{Chat: feature.Chat{GroupID: g}, Sender: aliceID, Text: "water the plants"}
	}
	if _, ok := c.Delivered(ctx, in(groupID)); !ok {
		t.Fatal("a listed group's message dropped")
	}
	if _, ok := c.Delivered(ctx, in(otherID)); ok {
		t.Fatal("an unlisted group's message delivered")
	}
	if out := logs.String(); !strings.Contains(out, "delivered message dropped: its group is not in SIGNAL_GROUP_IDS") || strings.Contains(out, otherID) {
		t.Fatalf("logs %q", out)
	}

	// Alice is an admin only of the unlisted group; Bob of the listed one.
	if c.dmAllowed(ctx, aliceID) || !c.dmAllowed(ctx, bobID) {
		t.Fatal("dmAllowed")
	}
	if !c.Reachable(ctx, feature.Chat{User: bobID}) || c.Reachable(ctx, feature.Chat{User: aliceID}) {
		t.Fatal("Reachable for a DM")
	}

	// The members of an unlisted group aren't loaded.
	c.members.on.Store(true)
	if gs, ok := c.members.load(ctx, t.Logf); !ok || len(gs) != 1 || gs[groupID] == nil {
		t.Fatalf("members %v %v", gs, ok)
	}
}

// Once connected with SIGNAL_GROUP_IDS all, the bot warns that it answers
// in any group, and suggests the line listing the groups it is in, with
// their names; once a run. With none, it says so; if listGroups fails, it
// warns without the line.
func TestSuggestGroups(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	// A newline in a name is collapsed; a backslash, which ending a line
	// in a systemd env file would join the next line to it, is shown as "/".
	f.set("listGroups", strings.NewReplacer(`"name":"Other"`, `"name":"Ot\nher"`, `"name":"Family"`, `"name":"Fam\\ily \\"`).Replace(twoGroups))
	c := newClient(f.sock, allow.Parse("none"), nil)
	c.suggest.Store(true)
	_, conn := run(t, c, f)
	line := "Signal: to answer only in the groups the bot is in now, set: SIGNAL_GROUP_IDS=" + groupID + "," + otherID + "  # Fam/ily /, Ot her\n"
	eventually(t, "the suggestion", func() bool { return strings.Contains(logs.String(), line) })
	if !strings.Contains(logs.String(), "Signal: the bot answers in any group anyone adds it to, as SIGNAL_GROUP_IDS is unset or all") {
		t.Fatalf("logs %q", logs)
	}
	conn.Close() // reconnects: not suggested again
	eventually(t, "the reconnect", func() bool { return len(f.calls("listAccounts")) == 2 })
	c.suggestGroups(context.Background())
	if n := strings.Count(logs.String(), "SIGNAL_GROUP_IDS="); n != 1 {
		t.Fatalf("suggested %d times", n)
	}

	before := len(logs.String())
	f.set("listGroups", `[{"id":"bGVmdA==","name":"Old","isMember":false}]`)
	c.suggest.Store(true)
	c.suggestGroups(context.Background())
	if out := logs.String()[before:]; !strings.Contains(out, "the bot is in no group yet") || strings.Contains(out, "SIGNAL_GROUP_IDS=") || strings.Contains(out, "Old") {
		t.Fatalf("logs %q", out)
	}

	before = len(logs.String())
	f.set("listGroups", `{"error":"x"`) // not even JSON: the call times out
	c.suggest.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	c.suggestGroups(ctx)
	if out := logs.String()[before:]; !strings.Contains(out, "answers in any group") || !strings.Contains(out, "listGroups failed, so no SIGNAL_GROUP_IDS is suggested") || strings.Contains(out, "SIGNAL_GROUP_IDS=") {
		t.Fatalf("logs %q", out)
	}
}

// With groups listed, nothing is suggested.
func TestNoSuggestionWhenListed(t *testing.T) {
	logs := captureLogs(t)
	t.Setenv("SIGNAL_DM_USERS", "none")
	t.Setenv("SIGNAL_GROUP_IDS", groupID)
	f := newFake(t)
	f.set("listGroups", twoGroups)
	t.Setenv("SIGNALCLI_SOCKET", f.sock)
	c := New(nil)
	c.names.refreshedAt = time.Now()
	run(t, c, f)
	c.suggestGroups(context.Background())
	if n := len(f.calls("listGroups")); n != 0 || strings.Contains(logs.String(), "any group") {
		t.Fatalf("%d listGroups, logs %q", n, logs)
	}
}

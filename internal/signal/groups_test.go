package signal

import (
	"context"
	"fmt"
	"maps"
	"strings"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/allow"
	"github.com/robdevops/signalbot/internal/kit/logx"
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
	if c := New(nil); !c.groupIDs.all || !c.report.Load() {
		t.Errorf("unset: %+v", c.groupIDs)
	}
	t.Setenv("SIGNAL_GROUP_IDS", groupID+","+otherID)
	if c := New(nil); c.groupIDs.all || len(c.groupIDs.ids) != 2 || !c.report.Load() {
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

// captureLevels is captureLogs with each line's priority, "<4>" a warning.
func captureLevels(t *testing.T) *logx.Buffer {
	t.Helper()
	var buf logx.Buffer
	t.Cleanup(logx.SetOutput(&buf, &buf, true))
	return &buf
}

// Once connected with SIGNAL_GROUP_IDS all, the bot warns that it answers
// in any group, lists the groups it is in, and suggests the line listing
// them, with their names; once a run. With none, it says so; if listGroups
// fails, it warns without the list or the line.
func TestReportGroupsAll(t *testing.T) {
	logs := captureLevels(t)
	f := newFake(t)
	// A newline in a name is collapsed; a backslash, which ending a line
	// in a systemd env file would join the next line to it, is shown as "/".
	f.set("listGroups", strings.NewReplacer(`"name":"Other"`, `"name":"Ot\nher"`, `"name":"Family"`, `"name":"Fam\\ily \\"`).Replace(twoGroups))
	c := newClient(f.sock, allow.Parse("none"), nil)
	c.report.Store(true)
	_, conn := run(t, c, f)
	line := "<4>Signal: to answer only in the groups the bot is in now, set: SIGNAL_GROUP_IDS=" + groupID + "," + otherID + "  # Fam/ily /, Ot her\n"
	eventually(t, "the suggestion", func() bool { return strings.Contains(logs.String(), line) })
	if out := logs.String(); !strings.Contains(out, "<4>Signal: the bot answers in any group anyone adds it to, as SIGNAL_GROUP_IDS is unset or all") ||
		!strings.Contains(out, "<5>Signal groups (2): Fam/ily / ("+groupID+"), Ot her ("+otherID+")\n") || strings.Contains(out, "ignored") {
		t.Fatalf("logs %q", out)
	}
	conn.Close() // reconnects: not listed or suggested again
	eventually(t, "the reconnect", func() bool { return len(f.calls("listAccounts")) == 2 })
	c.reportGroups(context.Background())
	if n, m := strings.Count(logs.String(), "SIGNAL_GROUP_IDS="), strings.Count(logs.String(), "Signal groups ("); n != 1 || m != 1 {
		t.Fatalf("suggested %d times, listed %d", n, m)
	}

	before := len(logs.String())
	f.set("listGroups", `[{"id":"bGVmdA==","name":"Old","isMember":false}]`)
	c.report.Store(true)
	c.reportGroups(context.Background())
	if out := logs.String()[before:]; !strings.Contains(out, "<5>Signal groups (0): none\n") || !strings.Contains(out, "<4>Signal: the bot is in no group yet") ||
		strings.Contains(out, "SIGNAL_GROUP_IDS=") || strings.Contains(out, "Old") {
		t.Fatalf("logs %q", out)
	}

	before = len(logs.String())
	f.set("listGroups", `{"error":"x"`) // not even JSON: the call times out
	c.report.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	c.reportGroups(ctx)
	if out := logs.String()[before:]; !strings.Contains(out, "answers in any group") ||
		!strings.Contains(out, "<4>Signal: listGroups failed, so the groups the bot is in aren't listed, nor SIGNAL_GROUP_IDS suggested") ||
		strings.Contains(out, "SIGNAL_GROUP_IDS=") || strings.Contains(out, "Signal groups (") {
		t.Fatalf("logs %q", out)
	}
}

// With groups listed, the bot lists the groups it is in once connected, as
// a notice while every one is listed; a group not listed is marked, and
// makes the line a warning. Nothing is suggested. If listGroups fails, it
// warns, naming no group.
func TestReportGroupsListed(t *testing.T) {
	logs := captureLevels(t)
	t.Setenv("SIGNAL_DM_USERS", "none")
	t.Setenv("SIGNAL_GROUP_IDS", groupID+","+otherID)
	f := newFake(t)
	f.set("listGroups", twoGroups)
	t.Setenv("SIGNALCLI_SOCKET", f.sock)
	c := New(nil)
	c.names.refreshedAt = time.Now()
	run(t, c, f)
	all := "<5>Signal groups (2): Family (" + groupID + "), Other (" + otherID + ")\n"
	eventually(t, "the list", func() bool { return strings.Contains(logs.String(), all) })
	c.reportGroups(context.Background()) // once a run
	if out := logs.String(); strings.Count(out, "Signal groups (") != 1 || strings.Contains(out, "any group") || strings.Contains(out, "SIGNAL_GROUP_IDS=") || strings.Contains(out, "<4>Signal groups") {
		t.Fatalf("logs %q", out)
	}

	// Each case is a client of its own, connected with its setting.
	connect := func(ids, groups string) *Client {
		f := newFake(t)
		f.set("listGroups", groups)
		c := newClient(f.sock, allow.Parse("none"), nil)
		c.groupIDs = parseGroups(ids)
		c.names.refreshedAt = time.Now()
		run(t, c, f)
		return c
	}

	before := len(logs.String())
	c = connect(groupID, twoGroups)
	c.report.Store(true)
	c.reportGroups(context.Background())
	want := "<4>Signal groups (2): Other (" + otherID + ") — not in SIGNAL_GROUP_IDS, ignored, Family (" + groupID + ")\n"
	if out := logs.String()[before:]; !strings.Contains(out, want) || strings.Count(out, "Signal groups (") != 1 {
		t.Fatalf("logs %q, want %q", out, want)
	}

	before = len(logs.String())
	c = connect("none", twoGroups)
	c.report.Store(true)
	c.reportGroups(context.Background())
	if out := logs.String()[before:]; !strings.Contains(out, "<4>Signal groups (2): ") || strings.Count(out, "not in SIGNAL_GROUP_IDS, ignored") != 2 {
		t.Fatalf("logs %q", out)
	}

	before = len(logs.String())
	c = connect(groupID, `{"error":"x"`) // not even JSON: the call times out
	c.report.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	c.reportGroups(ctx)
	if out := logs.String()[before:]; !strings.Contains(out, "<4>Signal: listGroups failed, so the groups the bot is in aren't listed: ") ||
		strings.Contains(out, "Family") || strings.Contains(out, "SIGNAL_GROUP_IDS=") || strings.Contains(out, "Signal groups (") {
		t.Fatalf("logs %q", out)
	}
}

// The startup line names at most groupsShown groups, the unlisted first,
// and counts the rest.
func TestReportGroupsCapped(t *testing.T) {
	logs := captureLevels(t)
	f := newFake(t)
	var gs []string
	for i := range groupsShown + 3 {
		gs = append(gs, fmt.Sprintf(`{"id":"g%d=","name":"G%d","isMember":true}`, i, i))
	}
	f.set("listGroups", "["+strings.Join(gs, ",")+"]")
	c := newClient(f.sock, allow.Parse("none"), nil)
	c.groupIDs = parseGroups("g0=")
	for i := 1; i < groupsShown+2; i++ {
		c.groupIDs.ids[fmt.Sprintf("g%d=", i)] = true
	}
	c.report.Store(true)
	run(t, c, f)
	last := fmt.Sprintf("G%d (g%d=) — not in SIGNAL_GROUP_IDS, ignored", groupsShown+2, groupsShown+2)
	eventually(t, "the list", func() bool { return strings.Contains(logs.String(), "Signal groups (") })
	out := logs.String()
	if !strings.Contains(out, fmt.Sprintf("<4>Signal groups (%d): %s, G0 (g0=), ", groupsShown+3, last)) || !strings.Contains(out, ", and 3 more\n") ||
		strings.Contains(out, fmt.Sprintf("G%d (", groupsShown+1)) {
		t.Fatalf("logs %q", out)
	}
}

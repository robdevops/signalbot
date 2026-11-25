package signal

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/allow"
)

// adminsOf is listGroups with the bot in a group whose admin is Alice, and
// out of one whose admin is Bob.
const adminsOf = `[{"id":"` + groupID + `","isMember":true,"members":[{"uuid":"` + aliceID + `"},{"uuid":"` + bobID + `"}],"admins":[{"number":"+1555","uuid":"` + aliceID + `"}]},` +
	`{"id":"bGVmdA==","isMember":false,"members":[{"uuid":"` + bobID + `"}],"admins":[{"uuid":"` + bobID + `"}]}]`

// By default an admin of a group the bot is a member of may DM it: decided
// in the message's slot, by asking signal-cli, and the answer kept, so the
// next DM, and a reaction, are decided on the reader. Anyone else's DM is
// refused, and logged by ID only; refused again, it costs no lookup.
func TestDMFromAnAdmin(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	f.set("listGroups", adminsOf)
	c := newClient(f.sock, allow.Parse(""), nil)
	c.names.refreshedAt = time.Now() // so loading names doesn't count
	got, conn := run(t, c, f)
	ctx := context.Background()
	dm := func(from, text string) bot.Received {
		receive(conn, `{"sourceUuid":"`+from+`","sourceName":"X","dataMessage":{"message":"`+text+`"}}`)
		return next(t, got)
	}

	r, ok := dm(aliceID, "hi").(bot.Incoming)
	if !ok {
		t.Fatalf("Alice's DM %+v", r)
	}
	if m, ok := r.Load(ctx, nil); !ok || !m.Addressed || m.Text != "hi" {
		t.Fatalf("Alice's DM loaded as %+v %v", m, ok)
	}
	if n := len(f.calls("listGroups")); n != 1 {
		t.Fatalf("%d listGroups", n)
	}
	r, _ = dm(aliceID, "again").(bot.Incoming)
	if m, ok := r.Load(ctx, nil); !ok || m.Text != "again" || len(f.calls("listGroups")) != 1 {
		t.Fatalf("Alice's second DM %+v %v, %d listGroups", m, ok, len(f.calls("listGroups")))
	}
	if r, ok := c.event(parse(t, `{"sourceUuid":"`+aliceID+`","dataMessage":{"reaction":{"emoji":"👍","targetAuthorUuid":"`+botID+`","targetSentTimestamp":5}}}`)); !ok {
		t.Fatalf("Alice's reaction dropped: %+v", r)
	}

	r, _ = dm(bobID, "let me in").(bot.Incoming)
	if _, ok := r.Load(ctx, nil); ok {
		t.Fatal("Bob, an admin only of a group the bot left, was answered")
	}
	if n := len(f.calls("listGroups")); n != 2 {
		t.Fatalf("%d listGroups", n)
	}
	if r, ok := c.event(parse(t, `{"sourceUuid":"`+bobID+`","dataMessage":{"message":"please"}}`)); ok {
		t.Fatalf("Bob's second DM reached the core: %+v", r)
	}
	if r, ok := c.event(parse(t, `{"sourceUuid":"`+bobID+`","dataMessage":{"reaction":{"emoji":"👍","targetAuthorUuid":"`+botID+`","targetSentTimestamp":5}}}`)); ok {
		t.Fatalf("Bob's reaction reached the core: %+v", r)
	}
	edit := parse(t, `{"sourceUuid":"`+bobID+`","timestamp":9,"editMessage":{"targetSentTimestamp":8,"dataMessage":{"message":"x"}}}`)
	if r, ok := c.event(edit); !ok || r != (bot.Deleted{Chat: feature.Chat{User: bobID}, Sender: bobID, Sent: 8, At: 9}) {
		t.Fatalf("Bob's edit %+v %v", r, ok)
	}
	if n := len(f.calls("listGroups")); n != 2 {
		t.Fatalf("%d listGroups", n)
	}
	// Refused twice, Bob is warned of once.
	if want := "Signal: a DM from " + bobID + " ignored: SIGNAL_DM_USERS doesn't allow it; add the ID to SIGNAL_DM_USERS to allow DMs from them\n"; strings.Count(logs.String(), want) != 1 || strings.Count(logs.String(), "a DM from") != 1 || strings.Contains(logs.String(), aliceID) {
		t.Fatalf("logs %q", logs)
	}

	// A group update forgets every verdict: Alice is asked about again.
	f.set("listGroups", `[]`)
	c.event(parse(t, `{"sourceUuid":"`+bobID+`","dataMessage":{"groupInfo":{"groupId":"`+groupID+`","type":"UPDATE"}}}`))
	r, _ = dm(aliceID, "still?").(bot.Incoming)
	if _, ok := r.Load(ctx, nil); ok {
		t.Fatal("Alice answered once no longer an admin")
	}
}

// Without admins in SIGNAL_DM_USERS, no one is asked about; with only
// mods, no one counts, as Signal groups have none; and a failed lookup
// refuses, with a warning naming no one.
func TestDMAdminsNotAsked(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	f.set("listGroups", adminsOf)
	for _, p := range []allow.Policy{allow.Parse("none"), allow.Parse("signal:" + bobID), allow.Parse("mods")} {
		c := newClient(f.sock, p, nil)
		connected(c)
		if r, ok := c.event(parse(t, `{"sourceUuid":"`+aliceID+`","dataMessage":{"message":"hi"}}`)); ok {
			t.Errorf("%v: Alice's DM %+v", p, r)
		}
	}
	if n := len(f.calls("listGroups")); n != 0 {
		t.Fatalf("%d listGroups", n)
	}
	if !newClient("x", allow.Parse("all"), nil).dmAllowed(context.Background(), bobID) {
		t.Fatal("all refused Bob")
	}

	before := len(logs.String())
	c := newClient(f.sock, allow.Parse("admins"), nil)
	run(t, c, f)
	f.set("listGroups", `{"error":"x"`) // not even JSON: the call times out
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if c.dmAllowed(ctx, aliceID) {
		t.Fatal("a failed lookup allowed Alice")
	}
	if out := logs.String()[before:]; !strings.Contains(out, "listGroups failed, so a DM's sender is taken for no admin") || strings.Contains(out, aliceID) {
		t.Fatalf("logs %q", out)
	}
	if _, known := c.verdicts.get(aliceID); known {
		t.Fatal("a failed lookup was kept")
	}
}

// A delivered DM, and a post to a DM, need its user allowed now, as an
// admin too; a drop names no one.
func TestDeliveredToAnAdmin(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	f.set("listGroups", adminsOf)
	c := newClient(f.sock, allow.Parse(""), nil)
	run(t, c, f)
	ctx := context.Background()
	if _, ok := c.Delivered(ctx, feature.Message{Chat: feature.Chat{User: aliceID}, Sender: aliceID, Text: "water the plants"}); !ok {
		t.Fatal("Alice's DM dropped")
	}
	if !c.Reachable(ctx, feature.Chat{User: aliceID}) || c.Reachable(ctx, feature.Chat{User: bobID}) {
		t.Fatal("Reachable")
	}
	if _, ok := c.Delivered(ctx, feature.Message{Chat: feature.Chat{User: bobID}, Sender: bobID, Text: "water the plants"}); ok {
		t.Fatal("Bob's DM delivered")
	}
	if out := logs.String(); !strings.Contains(out, "delivered message dropped: its sender may no longer DM the bot") || strings.Contains(out, bobID) {
		t.Fatalf("logs %q", out)
	}
}

// The verdicts held are bounded: once full, the stale go, or all.
func TestVerdictsBounded(t *testing.T) {
	var v verdicts
	for i := range verdictMax + 5 {
		v.put(0, strings.Repeat("x", i+1), true)
	}
	if len(v.m) > verdictMax {
		t.Fatalf("%d held", len(v.m))
	}
	v.m["old"] = verdict{true, time.Now().Add(-verdictAge - time.Second)}
	if _, ok := v.get("old"); ok {
		t.Fatal("a stale verdict believed")
	}
}

// An answer asked for before a group update isn't kept after it, as an
// admin may have gone meanwhile.
func TestVerdictsForgottenWhileAsking(t *testing.T) {
	var v verdicts
	gen := v.generation()
	v.forget()
	v.put(gen, aliceID, true)
	if _, ok := v.get(aliceID); ok {
		t.Fatal("an answer from before the update kept")
	}
	v.put(v.generation(), aliceID, true)
	if admin, ok := v.get(aliceID); !ok || !admin {
		t.Fatal("a current answer not kept")
	}
}

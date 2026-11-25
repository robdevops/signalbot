package discord

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/allow"
)

// dmFrom is a DM from user, saying text.
func dmFrom(user, text string) json.RawMessage {
	return json.RawMessage(`{"id":"1200000000000000101","type":0,` + inDM + `,"author":{"id":"` + user + `","username":"u"},"content":"` + text + `"}`)
}

// tryLoad is r's message, and whether Load took it.
func tryLoad(t *testing.T, r bot.Received) bool {
	t.Helper()
	in, ok := r.(bot.Incoming)
	if !ok {
		t.Fatalf("not Incoming: %#v", r)
	}
	_, ok = in.Load(context.Background(), func(string, string) {})
	return ok
}

// setRole gives TestGroups' role the permissions perms.
func setRole(c *Client, perms string) {
	c.events("GUILD_ROLE_UPDATE", json.RawMessage(`{"guild_id":"`+guild+`","role":{"id":"`+roleID+`","permissions":"`+perms+`"}}`))
}

// By default only a server's admins and mods may DM the bot: a mod by a
// role's Manage Messages, an admin as the owner, each looked up in the
// message's slot and the answer kept, so the next DM is decided on the
// reader; anyone else's DM is refused and logged by ID, never its text.
func TestDMAdminsAndMods(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	c := groupsClient(f)
	c.dm = allow.Parse("")
	setRole(c, "8192") // MANAGE_MESSAGES: Bob is a mod

	if tryLoad(t, one(t, c.events("MESSAGE_CREATE", dmFrom(alice, "secret words")))) {
		t.Fatal("Alice, neither, answered")
	}
	if !strings.Contains(logs.String(), prefix+alice+" ignored because not allowed by DISCORD_DM_USERS") || strings.Contains(logs.String(), "secret words") {
		t.Fatalf("log %q", logs.String())
	}
	if rs := c.events("MESSAGE_CREATE", dmFrom(alice, "again")); len(rs) != 0 {
		t.Fatalf("Alice's next DM, decided on the reader: %+v", rs)
	}
	if n := len(f.callsTo(aliceGet)); n != 1 {
		t.Fatalf("looked Alice up %d times", n)
	}
	if !tryLoad(t, one(t, c.events("MESSAGE_CREATE", dmFrom(bob, "hi")))) {
		t.Fatal("Bob, a mod, refused")
	}
	if m := load(t, one(t, c.events("MESSAGE_CREATE", dmFrom(bob, "hi")))); m.Chat.User != prefix+bob {
		t.Fatalf("Bob's next %+v", m)
	}

	// Only admins: Bob, a mod, isn't one; Alice is, once she owns the
	// server, which is asked again.
	c.dm = allow.Parse("admins")
	if rs := c.events("MESSAGE_CREATE", dmFrom(bob, "hi")); len(rs) != 0 {
		t.Fatalf("Bob, only a mod: %+v", rs)
	}
	c.events("GUILD_UPDATE", json.RawMessage(`{"id":"`+guild+`","owner_id":"`+alice+`"}`))
	if !tryLoad(t, one(t, c.events("MESSAGE_CREATE", dmFrom(alice, "hi")))) {
		t.Fatal("Alice, the owner, refused")
	}
	// Manage Server makes an admin, but not a mod.
	c.events("GUILD_UPDATE", json.RawMessage(`{"id":"`+guild+`","owner_id":"700000000000000001"}`))
	setRole(c, "32")
	if !tryLoad(t, one(t, c.events("MESSAGE_CREATE", dmFrom(bob, "hi")))) {
		t.Fatal("Bob, with Manage Server, refused")
	}
	c.dm = allow.Parse("mods")
	if ok, known := c.dmDecided(bob); !known || ok {
		t.Fatalf("Bob, with Manage Server, a mod: %v %v", ok, known)
	}
}

// A listed ID, with "discord:" or without, or "all", is answered with no
// lookup; "none" refuses everyone, also with no lookup.
func TestDMListed(t *testing.T) {
	f := newFake(t)
	c := groupsClient(f)
	for _, v := range []string{alice, prefix + alice, "none " + alice, "all"} {
		c.dm = allow.Parse(v)
		if !tryLoad(t, one(t, c.events("MESSAGE_CREATE", dmFrom(alice, "hi")))) {
			t.Fatalf("%q: Alice refused", v)
		}
	}
	c.dm = allow.Parse(alice)
	if rs := c.events("MESSAGE_CREATE", dmFrom(bob, "hi")); len(rs) != 0 {
		t.Fatalf("Bob, not listed: %+v", rs)
	}
	c.dm = allow.Parse("none")
	if rs := c.events("MESSAGE_CREATE", dmFrom(alice, "hi")); len(rs) != 0 {
		t.Fatalf("none: %+v", rs)
	}
	if r := f.routes(); len(r) != 0 {
		t.Fatalf("looked up %v", r)
	}
	// In a channel, anyone is answered.
	if m := load(t, one(t, c.events("MESSAGE_CREATE", json.RawMessage(`{"id":"1200000000000000102","type":0,`+inGuild+`,`+fromBob+`,"content":"<@`+botID+`> hi"}`)))); !m.Addressed {
		t.Fatalf("in a channel %+v", m)
	}
}

// A lookup that fails refuses the DM, with a warning naming no one, and
// isn't kept: the next DM asks again.
func TestDMLookupFails(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	c := groupsClient(f)
	c.dm = allow.Parse("")
	setRole(c, "8192")
	f.errs[bobGet] = []int{500}
	if tryLoad(t, one(t, c.events("MESSAGE_CREATE", dmFrom(bob, "hi")))) {
		t.Fatal("answered when the lookup failed")
	}
	if l := logs.String(); !strings.Contains(l, "so the DM is refused") || strings.Count(l, bob) != 1 || strings.Contains(l, guild) {
		t.Fatalf("log %q", l) // Bob's ID only in the refusal
	}
	if !tryLoad(t, one(t, c.events("MESSAGE_CREATE", dmFrom(bob, "hi")))) {
		t.Fatal("refused once the lookup works")
	}
}

// In a DM, a reaction and a press count only from someone who may DM the
// bot as far as the reader can tell: listed, or with a fresh verdict.
func TestDMReactionsAndPresses(t *testing.T) {
	f := newFake(t)
	c := groupsClient(f)
	c.dm = allow.Parse("")
	setRole(c, "8192")
	c.Watch(bot.Watch{Events: true})
	react := func() []bot.Received {
		return c.events("MESSAGE_REACTION_ADD", json.RawMessage(`{"user_id":"`+bob+`",`+inDM+`,"message_id":"1200000000000000101","emoji":{"id":null,"name":"👍"}}`))
	}
	press := func(n string) []bot.Received {
		return c.events("INTERACTION_CREATE", json.RawMessage(`{"id":"125000000000000000`+n+`","application_id":"app1","type":3,"token":"t`+n+`",`+inDM+
			`,"user":{"id":"`+bob+`","username":"bob"},"message":{"id":"1200000000000000004"},"data":{"custom_id":"a","component_type":2}}`))
	}
	if rs := react(); len(rs) != 0 {
		t.Fatalf("reaction with no verdict %+v", rs)
	}
	if rs := press("1"); len(rs) != 0 {
		t.Fatalf("press with no verdict %+v", rs)
	}
	if a := f.waitCalls("POST /interactions/1250000000000000001/t1/callback", 1)[0]; a.Body["type"] != float64(6) {
		t.Fatalf("ack %+v", a)
	}
	tryLoad(t, one(t, c.events("MESSAGE_CREATE", dmFrom(bob, "hi")))) // Bob's verdict, a mod
	if _, ok := one(t, react()).(bot.Happened); !ok {
		t.Fatal("reaction from a mod dropped")
	}
	if _, ok := one(t, press("2")).(bot.Pressed); !ok {
		t.Fatal("press from a mod dropped")
	}
}

// A slash command in a DM from someone who may not DM the bot has its
// "thinking…" taken away and nothing said, whether decided on the reader
// or after a lookup.
func TestDMSlashCommandRefused(t *testing.T) {
	f := newFake(t)
	c := groupsClient(f)
	c.dm = allow.Parse("")
	cmd := func(n string) []bot.Received {
		return c.events("INTERACTION_CREATE", json.RawMessage(`{"id":"125000000000000000`+n+`","application_id":"app1","type":2,"token":"t`+n+`",`+inDM+
			`,"user":{"id":"`+alice+`","username":"alice"},"data":{"name":"hello","type":1}}`))
	}
	if tryLoad(t, one(t, cmd("1"))) {
		t.Fatal("Alice's slash command answered")
	}
	f.waitCalls("DELETE /webhooks/app1/t1/messages/@original", 1)
	if rs := cmd("2"); len(rs) != 0 {
		t.Fatalf("Alice's next, decided on the reader: %+v", rs)
	}
	if a := f.waitCalls("POST /interactions/1250000000000000002/t2/callback", 1)[0]; a.Body["type"] != float64(5) {
		t.Fatalf("ack %+v", a)
	}
	f.waitCalls("DELETE /webhooks/app1/t2/messages/@original", 1)
	time.Sleep(20 * time.Millisecond)
	for _, r := range f.routes() {
		if strings.HasPrefix(r, "PATCH ") || strings.HasPrefix(r, "POST /channels/") || strings.HasPrefix(r, "POST /webhooks/") {
			t.Fatalf("said something: %v", f.routes())
		}
	}
}

// A post delivered in a DM goes out only while its user may DM the bot:
// once their role no longer makes them a mod, it is dropped, and the log
// names no one.
func TestDeliveredDM(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	c := groupsClient(f)
	c.dm = allow.Parse("")
	setRole(c, "8192")
	deliver := func() bool {
		_, ok := c.Delivered(context.Background(), feature.Message{Chat: feature.Chat{User: prefix + bob}, Sender: prefix + bob, Text: "hello"})
		return ok
	}
	if !deliver() {
		t.Fatal("not delivered for a mod")
	}
	setRole(c, "0")
	if deliver() {
		t.Fatal("delivered once Bob is no longer a mod")
	}
	if l := logs.String(); !strings.Contains(l, "delivered message dropped: its sender may no longer DM the bot") || strings.Contains(l, bob) {
		t.Fatalf("log %q", l)
	}
}

// Groups marks each channel Admin or Mod by the member's permissions
// there, overwrites counted; the owner is both.
func TestGroupsAdminMod(t *testing.T) {
	f := newFake(t)
	c := groupsClient(f)
	setRole(c, "8192")
	c.events("CHANNEL_UPDATE", json.RawMessage(`{"id":"`+secret+`","guild_id":"`+guild+`","name":"secret","type":0,"permission_overwrites":[
		{"id":"`+guild+`","type":0,"allow":"0","deny":"1024"},{"id":"`+roleID+`","type":0,"allow":"1024","deny":"8192"}]}`))
	marks := func(user string) map[string][2]bool {
		gs, err := c.Groups(context.Background(), prefix+user)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string][2]bool{}
		for _, g := range gs {
			out[g.Name] = [2]bool{g.Admin, g.Mod}
		}
		return out
	}
	b := marks(bob)
	if b["#general (Test server)"] != [2]bool{false, true} || b["#secret (Test server)"] != [2]bool{false, false} {
		t.Fatalf("Bob's %v", b)
	}
	if a := marks(alice); a["#general (Test server)"] != [2]bool{} {
		t.Fatalf("Alice's %v", a)
	}
	c.events("GUILD_UPDATE", json.RawMessage(`{"id":"`+guild+`","owner_id":"`+alice+`"}`))
	if a := marks(alice); a["#general (Test server)"] != [2]bool{true, true} || a["#secret (Test server)"] != [2]bool{true, true} {
		t.Fatalf("the owner's %v", a)
	}
}

// A verdict looked up while a server's roles change isn't kept: it may
// be from before the change.
func TestDMVerdictForgottenMidLookup(t *testing.T) {
	f := newFake(t)
	c := groupsClient(f)
	c.dm = allow.Parse("")
	c.mu.Lock()
	gen := c.forgets
	c.mu.Unlock()
	setRole(c, "8192") // while Bob's lookup was out
	c.keepVerdict(bob, verdict{at: time.Now()}, gen)
	if _, known := c.dmDecided(bob); known {
		t.Fatal("a verdict from before the change kept")
	}
	if !tryLoad(t, one(t, c.events("MESSAGE_CREATE", dmFrom(bob, "hi")))) {
		t.Fatal("Bob, a mod now, refused")
	}
	if ok, known := c.dmDecided(bob); !ok || !known {
		t.Fatalf("Bob's verdict after the change: %v %v", ok, known)
	}
}

// A DM refused on the reader leaves nothing behind: not its channel, nor
// its sender as the author of its message.
func TestDMRefusedNotKept(t *testing.T) {
	f := newFake(t)
	c := groupsClient(f)
	c.dm = allow.Parse(bob)
	if rs := c.events("MESSAGE_CREATE", dmFrom(alice, "hi")); len(rs) != 0 {
		t.Fatalf("Alice, not listed: %+v", rs)
	}
	c.mu.Lock()
	_, dm := c.dms[alice]
	c.mu.Unlock()
	if dm || c.authorOf(1200000000000000101) != "" {
		t.Fatal("kept something of Alice's refused DM")
	}
}

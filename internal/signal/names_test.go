package signal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestContactBest(t *testing.T) {
	var cs []contact
	json.Unmarshal([]byte(`[{"number":null,"uuid":"`+aliceID+`","username":"alice.20","givenName":null,"profile":{"givenName":"Alice"}}]`), &cs)
	if r := cs[0].best(); r != (rankedName{"Alice", rankGiven}) {
		t.Errorf("profile name: %+v", r)
	}
	k := contact{GivenName: "Alice B", Username: "alice.1"}
	k.Profile.GivenName = "Alice"
	if r := k.best(); r.name != "Alice B" {
		t.Errorf("the owner's name for the contact should win: %+v", r)
	}
	if r := (contact{Username: "alice.1"}).best(); r != (rankedName{"alice.1", rankUsername}) {
		t.Errorf("username: %+v", r)
	}
	if r := (contact{GivenName: " \n "}).best(); r.rank != 0 {
		t.Errorf("blank: %+v", r)
	}
}

func TestResolveRanks(t *testing.T) {
	c := New("x")
	m := Mention{Name: aliceID, UUID: aliceID}
	if got := c.resolve(m); got != "" {
		t.Fatalf("an unknown ID must resolve to nothing, not the ID: %q", got)
	}
	if got := c.resolve(Mention{Name: "Alice\nB", UUID: aliceID}); got != "Alice B" {
		t.Fatalf("the mention's own name, made safe: %q", got)
	}
	c.names.replace([]contact{{UUID: aliceID, Username: "alice.20"}})
	if got := c.resolve(m); got != "alice.20" {
		t.Fatalf("username: %q", got)
	}
	if got := c.resolve(Mention{Name: "Ally", UUID: aliceID}); got != "Ally" {
		t.Fatalf("the mention's name beats a username: %q", got)
	}
	c.names.minRefresh = time.Hour
	c.names.refreshedAt = time.Now() // so LoadNames doesn't try to load
	c.LoadNames(context.Background(), &Envelope{SourceUUID: aliceID, SourceName: "Alice S"})
	if got := c.resolve(m); got != "Alice S" {
		t.Fatalf("a sender's name beats a username: %q", got)
	}
	k := contact{UUID: aliceID}
	k.Profile.GivenName = "Alice"
	c.names.replace([]contact{k})
	c.LoadNames(context.Background(), &Envelope{SourceUUID: aliceID, SourceName: "Not Alice"})
	if got := c.resolve(m); got != "Alice" {
		t.Fatalf("a sender's name must not replace a profile name: %q", got)
	}
}

func TestNamesCapped(t *testing.T) {
	n := newNames()
	for i := range maxNames + 10 {
		n.set(rankedName{"Bob", rankSeen}, fmt.Sprint(i))
	}
	if len(n.byID) != maxNames {
		t.Fatalf("%d names kept", len(n.byID))
	}
}

// The contact list is loaded on connect and again when a group changes,
// once for a burst, and each load replaces the names: someone who left is
// forgotten.
func TestContactsReloadOnConnectAndGroupChange(t *testing.T) {
	captureLogs(t)
	f := newFake(t)
	f.set("listContacts", `[{"uuid":"`+bobID+`","givenName":"Bob"}]`)
	c := New(f.sock)
	c.names.minRefresh = 50 * time.Millisecond
	got, conn := run(t, c, f)
	eventually(t, "the load on connect", func() bool { return c.Name(bobID) == "Bob" })

	f.set("listContacts", `[{"uuid":"`+aliceID+`","givenName":"Alice"}]`)
	update := `{"sourceUuid":"` + aliceID + `","dataMessage":{"groupInfo":{"groupId":"` + groupID + `","type":"UPDATE"}}}`
	for range 5 {
		receive(conn, update)
	}
	eventually(t, "the load after the change", func() bool { return c.Name(aliceID) == "Alice" })
	if n := c.Name(bobID); n != "" {
		t.Fatalf("someone who left is still known as %q", n)
	}
	for range 5 {
		next(t, got) // the updates still reach the core
	}
	time.Sleep(150 * time.Millisecond)
	if n := len(f.calls("listContacts")); n != 2 {
		t.Fatalf("%d loads, want 2", n)
	}
	receive(conn, `{"sourceUuid":"`+aliceID+`","dataMessage":{"message":"hi","groupInfo":{"groupId":"`+groupID+`"}}}`)
	next(t, got)
	time.Sleep(100 * time.Millisecond)
	if n := len(f.calls("listContacts")); n != 2 {
		t.Fatalf("an ordinary message caused a load: %d", n)
	}
}

// A mention of someone known, with a fresh list, loads nothing; a list over
// a day old, or a mention with no name, is loaded again, at most every
// minRefresh.
func TestLoadNamesOnMention(t *testing.T) {
	captureLogs(t)
	f := newFake(t)
	f.set("listContacts", `[{"uuid":"`+bobID+`","givenName":"Bob"},{"uuid":"`+botID+`","givenName":"Bot"}]`)
	c := New(f.sock)
	c.names.minRefresh = 30 * time.Millisecond
	run(t, c, f)
	eventually(t, "the load on connect", func() bool { return c.Name(bobID) == "Bob" })
	ctx := context.Background()
	mention := func(id string) *Envelope {
		return &Envelope{SourceUUID: aliceID, SourceName: "Alice", DataMessage: &DataMessage{Message: "￼ hi ￼",
			Mentions: []Mention{{UUID: botID, Start: 0, Length: 1}, {UUID: id, Start: 5, Length: 1}}}}
	}
	loads := func() int { return len(f.calls("listContacts")) }

	time.Sleep(40 * time.Millisecond)
	c.LoadNames(ctx, mention(bobID))
	if loads() != 1 {
		t.Fatalf("a fresh list was loaded again: %d", loads())
	}
	if c.Name(aliceID) != "Alice" {
		t.Fatal("the sender's name was not learned")
	}
	c.names.mu.Lock()
	c.names.loadedAt = time.Now().Add(-25 * time.Hour)
	c.names.mu.Unlock()
	c.LoadNames(ctx, mention(bobID))
	if loads() != 2 {
		t.Fatalf("a stale list was not loaded: %d", loads())
	}
	c.LoadNames(ctx, mention("unknown"))
	if loads() != 2 {
		t.Fatalf("loaded again within minRefresh: %d", loads())
	}
	time.Sleep(40 * time.Millisecond)
	c.LoadNames(ctx, mention("unknown"))
	if loads() != 3 {
		t.Fatalf("an unknown mention did not load: %d", loads())
	}
}

func TestBotName(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	c := New(f.sock)
	c.names.minRefresh = time.Millisecond
	run(t, c, f)
	time.Sleep(5 * time.Millisecond)
	c.LoadNames(context.Background(), &Envelope{SourceUUID: aliceID})
	if got := c.BotName(); got != "signalbot" {
		t.Fatalf("no profile name: %q", got)
	}
	if !strings.Contains(logs.String(), "own profile name not found") {
		t.Fatalf("no warning: %q", logs.String())
	}
	f.set("listContacts", `[{"uuid":"`+botID+`","givenName":"Bot"}]`)
	time.Sleep(5 * time.Millisecond)
	c.LoadNames(context.Background(), &Envelope{SourceUUID: aliceID})
	if got := c.BotName(); got != "Bot" {
		t.Fatalf("profile name: %q", got)
	}
}

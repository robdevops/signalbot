package signal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/config"
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
	c := newClient("x", nil, nil)
	m := mention{Name: aliceID, UUID: aliceID}
	if got := c.resolve(m); got != "" {
		t.Fatalf("an unknown ID must resolve to nothing, not the ID: %q", got)
	}
	if got := c.resolve(mention{Name: "Alice\nB", UUID: aliceID}); got != "Alice B" {
		t.Fatalf("the mention's own name, made safe: %q", got)
	}
	c.names.replace([]contact{{UUID: aliceID, Username: "alice.20"}})
	if got := c.resolve(m); got != "alice.20" {
		t.Fatalf("username: %q", got)
	}
	if got := c.resolve(mention{Name: "Ally", UUID: aliceID}); got != "Ally" {
		t.Fatalf("the mention's name beats a username: %q", got)
	}
	c.names.refreshedAt = time.Now() // so LoadNames doesn't try to load
	c.loadNames(context.Background(), &envelope{SourceUUID: aliceID, SourceName: "Alice S"})
	if got := c.resolve(m); got != "Alice S" {
		t.Fatalf("a sender's name beats a username: %q", got)
	}
	k := contact{UUID: aliceID}
	k.Profile.GivenName = "Alice"
	c.names.replace([]contact{k})
	c.loadNames(context.Background(), &envelope{SourceUUID: aliceID, SourceName: "Not Alice"})
	if got := c.resolve(m); got != "Alice" {
		t.Fatalf("a sender's name must not replace a profile name: %q", got)
	}
}

// A person's number is known from the contact list, or from a message they
// sent, so an allowlist of numbers can be checked against an ACI. Each load
// replaces them, and they are capped.
func TestNumber(t *testing.T) {
	c := newClient("x", nil, nil)
	c.names.replace([]contact{{UUID: aliceID, Number: "+1555"}, {UUID: bobID}})
	if c.number(aliceID) != "+1555" || c.number(bobID) != "" || c.number("") != "" {
		t.Fatalf("numbers %v", c.names.numbers)
	}
	c.names.refreshedAt = time.Now() // so LoadNames doesn't try to load
	c.loadNames(context.Background(), &envelope{SourceUUID: bobID, SourceNumber: "+1666"})
	if c.number(bobID) != "+1666" {
		t.Fatalf("numbers %v", c.names.numbers)
	}
	c.names.replace(nil)
	if c.number(aliceID) != "" {
		t.Fatalf("numbers %v", c.names.numbers)
	}
	for i := range maxNames + 10 {
		c.names.learn(rankedName{}, fmt.Sprint(i), "+1")
	}
	if len(c.names.numbers) != maxNames {
		t.Fatalf("%d numbers kept", len(c.names.numbers))
	}
}

// gapClient is a client that loads the contact list at most every gap.
func gapClient(sock string, gap time.Duration) *Client {
	return newClient(sock, nil, &config.Test{ReloadGap: gap})
}

// The test hooks: a contact load counts as work until it ends, and each
// frame received calls Handled.
func TestTestHooks(t *testing.T) {
	captureLogs(t)
	f := newFake(t)
	delete(f.res, "listContacts") // never answered
	var frames atomic.Int32
	test := &config.Test{Handled: func() { frames.Add(1) }}
	c := newClient(f.sock, nil, test)
	_, conn := run(t, c, f)
	eventually(t, "the load on connect", func() bool { return len(f.calls("listContacts")) == 1 })
	if test.Idle() {
		t.Fatal("idle while the contacts load")
	}
	receive(conn, `{"sourceUuid":"`+aliceID+`","dataMessage":{"message":"hi"}}`)
	eventually(t, "the frame", func() bool { return frames.Load() == 1 })
	conn.Close()
	eventually(t, "the load to end", test.Idle)
}

func TestNamesCapped(t *testing.T) {
	n := &newClient("x", nil, nil).names
	for i := range maxNames + 10 {
		n.learn(rankedName{"Bob", rankSeen}, fmt.Sprint(i), "")
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
	c := gapClient(f.sock, 50*time.Millisecond)
	got, conn := run(t, c, f)
	eventually(t, "the load on connect", func() bool { return c.name(bobID) == "Bob" })

	f.set("listContacts", `[{"uuid":"`+aliceID+`","givenName":"Alice"}]`)
	update := `{"sourceUuid":"` + aliceID + `","dataMessage":{"groupInfo":{"groupId":"` + groupID + `","type":"UPDATE"}}}`
	for range 5 {
		receive(conn, update)
	}
	eventually(t, "the load after the change", func() bool { return c.name(aliceID) == "Alice" })
	if n := c.name(bobID); n != "" {
		t.Fatalf("someone who left is still known as %q", n)
	}
	for range 5 {
		if r, ok := next(t, got).(bot.GroupChanged); !ok {
			t.Fatalf("got %+v", r) // the changes still reach the core
		}
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
	c := gapClient(f.sock, 30*time.Millisecond)
	run(t, c, f)
	eventually(t, "the load on connect", func() bool { return c.name(bobID) == "Bob" })
	ctx := context.Background()
	mention := func(id string) *envelope {
		return &envelope{SourceUUID: aliceID, SourceName: "Alice", DataMessage: &dataMessage{Message: "￼ hi ￼",
			Mentions: []mention{{UUID: botID, Start: 0, Length: 1}, {UUID: id, Start: 5, Length: 1}}}}
	}
	loads := func() int { return len(f.calls("listContacts")) }

	time.Sleep(40 * time.Millisecond)
	c.loadNames(ctx, mention(bobID))
	if loads() != 1 {
		t.Fatalf("a fresh list was loaded again: %d", loads())
	}
	if c.name(aliceID) != "Alice" {
		t.Fatal("the sender's name was not learned")
	}
	c.names.mu.Lock()
	c.names.loadedAt = time.Now().Add(-25 * time.Hour)
	c.names.mu.Unlock()
	c.loadNames(ctx, mention(bobID))
	if loads() != 2 {
		t.Fatalf("a stale list was not loaded: %d", loads())
	}
	c.loadNames(ctx, mention("unknown"))
	if loads() != 2 {
		t.Fatalf("loaded again within minRefresh: %d", loads())
	}
	time.Sleep(40 * time.Millisecond)
	c.loadNames(ctx, mention("unknown"))
	if loads() != 3 {
		t.Fatalf("an unknown mention did not load: %d", loads())
	}
}

func TestBotName(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	c := gapClient(f.sock, time.Millisecond)
	run(t, c, f)
	time.Sleep(5 * time.Millisecond)
	c.loadNames(context.Background(), &envelope{SourceUUID: aliceID})
	if got := c.BotName(); got != "signalbot" {
		t.Fatalf("no profile name: %q", got)
	}
	if !strings.Contains(logs.String(), "own profile name not found") {
		t.Fatalf("no warning: %q", logs.String())
	}
	f.set("listContacts", `[{"uuid":"`+botID+`","givenName":"Bot"}]`)
	time.Sleep(5 * time.Millisecond)
	c.loadNames(context.Background(), &envelope{SourceUUID: aliceID})
	if got := c.BotName(); got != "Bot" {
		t.Fatalf("profile name: %q", got)
	}
}

// Redact scrubs every name the bot knows but its own, as whole words and
// ignoring capitals, longest first, and every UUID and phone number.
func TestRedact(t *testing.T) {
	c := newClient("x", nil, nil)
	c.id.Store(botID)
	c.names.learn(rankedName{"Alice", rankContact}, aliceID, "")
	c.names.learn(rankedName{"Alice Smith", rankContact}, "u-as", "")
	c.names.learn(rankedName{"Bob", rankSeen}, bobID, "")
	c.names.learn(rankedName{"José", rankGiven}, "u-j", "")
	c.names.learn(rankedName{"Botty", rankGiven}, botID, "")
	for in, want := range map[string]string{
		"Alice's file":                      "redacted's file",
		"alice smith asked; ALICE agreed":   "redacted asked; redacted agreed",
		"Malice and Bobby stay":             "Malice and Bobby stay",
		"José, then Bob.":                   "redacted, then redacted.",
		"Botty is the bot":                  "Botty is the bot",
		"user " + aliceID + " at +15550001": "user redacted at redacted",
		"":                                  "",
	} {
		if got := c.Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}

package signal

import (
	"bufio"
	"context"
	"github.com/robdevops/signalbot/internal/chat"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/transports/signal/signaltest"
)

const (
	bobID = "99999999-8888-7777-6666-555555555555"
	eveID = "55555555-4444-3333-2222-111111111111"
)

// groupUpdate is the event signal-cli sends when a group's members, name or
// description change.
const groupUpdate = `{"envelope":{"source":"` + alice + `","sourceUuid":"` + alice + `","sourceName":"Alice",` +
	`"dataMessage":{"groupInfo":{"groupId":"` + group + `","groupName":"g","revision":9,"type":"UPDATE"}}},"account":"` + self + `"}`

// The contact list is loaded when the bot connects and again whenever a group
// changes, and each load replaces the cache: a member who left is forgotten and
// a new one is known before they say anything.
func TestContactsReloadOnConnectAndGroupChange(t *testing.T) {
	var mu sync.Mutex
	calls, members := 0, `[{"uuid":"`+bobID+`","givenName":"Bob"}]`
	conns := make(chan net.Conn, 1)
	sock := signaltest.Socket(t, func(_ int, c net.Conn, r *bufio.Reader) {
		conns <- c
		signaltest.RPC(c, r, func(m string) (string, bool) {
			mu.Lock()
			defer mu.Unlock()
			switch m {
			case "listAccounts":
				return `[{"number":null,"aci":"` + self + `"}]`, true
			case "listContacts":
				calls++
				return members, true
			}
			return "", false
		})
	})
	a := NewAdapter(sock, nil)
	a.names.minRefresh = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx, func(chat.Incoming) {})
	c := <-conns

	wait := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	count := func() int { mu.Lock(); defer mu.Unlock(); return calls }

	wait("the load on connect", func() bool { return a.names.lookup(bobID).name == "Bob" })

	// Bob leaves and Eve joins; the group reports an update.
	mu.Lock()
	members = `[{"uuid":"` + eveID + `","givenName":"Eve"}]`
	mu.Unlock()
	for range 5 { // a burst of changes
		c.Write([]byte(receiveLine(groupUpdate)))
	}
	wait("the reload after the group change", func() bool { return a.names.lookup(eveID).name == "Eve" })
	if got := a.names.lookup(bobID).name; got != "" {
		t.Fatalf("a member who left is still known as %q", got)
	}
	time.Sleep(200 * time.Millisecond)
	if n := count(); n != 2 {
		t.Fatalf("%d contact loads, want 2 (connect, then one for the whole burst)", n)
	}

	// An ordinary message that is not for the bot changes nothing.
	c.Write([]byte(receiveLine(groupPlain)))
	time.Sleep(200 * time.Millisecond)
	if n := count(); n != 2 {
		t.Fatalf("an unaddressed message caused a contact load: %d loads", n)
	}
}

// A mention reloads a contact list that is over a day old, once; a fresh one
// is left alone.
func TestContactsOverADayOldReloadOnAMention(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	conns := make(chan net.Conn, 1)
	sock := signaltest.Socket(t, func(_ int, c net.Conn, r *bufio.Reader) {
		conns <- c
		signaltest.RPC(c, r, func(m string) (string, bool) {
			mu.Lock()
			defer mu.Unlock()
			switch m {
			case "listAccounts":
				return `[{"number":null,"aci":"` + self + `"}]`, true
			case "listContacts":
				calls++
				// The bot's own name is known, so answering never reloads.
				return `[{"uuid":"` + bobID + `","givenName":"Bob"},{"uuid":"` + self + `","givenName":"Botty"}]`, true
			}
			return "", false
		})
	})
	a := NewAdapter(sock, nil)
	a.names.minRefresh = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Loading an addressed message is where an unknown or stale name reloads.
	go a.Run(ctx, func(in chat.Incoming) {
		if in.Addressed {
			go in.Load(ctx)
		}
	})
	c := <-conns
	count := func() int { mu.Lock(); defer mu.Unlock(); return calls }
	wait := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s", what)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	wait("the load on connect", func() bool { return count() == 1 && a.names.lookup(bobID).name == "Bob" })

	// "@bot hi @Bob": Bob is known, so a fresh cache needs no reload.
	mentionBob := `{"envelope":{"source":"` + alice + `","sourceUuid":"` + alice + `","sourceName":"Alice","dataMessage":{"message":"￼ hi ￼",` +
		`"mentions":[{"uuid":"` + self + `","start":0,"length":1},{"uuid":"` + bobID + `","start":5,"length":1}],"groupInfo":{"groupId":"` + group + `"}}},"account":"` + self + `"}`
	c.Write([]byte(receiveLine(mentionBob)))
	time.Sleep(300 * time.Millisecond)
	if n := count(); n != 1 {
		t.Fatalf("a fresh contact list was reloaded: %d loads", n)
	}

	a.names.mu.Lock()
	a.names.loadedAt = time.Now().Add(-25 * time.Hour)
	a.names.mu.Unlock()
	c.Write([]byte(receiveLine(mentionBob)))
	wait("the reload of the stale list", func() bool { return count() == 2 })
	time.Sleep(300 * time.Millisecond)
	c.Write([]byte(receiveLine(mentionBob)))
	time.Sleep(300 * time.Millisecond)
	if n := count(); n != 2 {
		t.Fatalf("%d loads, want 2: the reload should have made the list fresh", n)
	}
}

// A group change drops the names remembered for that group, and only that one.
func TestGroupChangeForgetsThatChatsShownNames(t *testing.T) {
	a := testAdapter(nil)
	a.known[groupChat(group)] = map[string]string{"bob": bobID}
	a.known[groupChat("other")] = map[string]string{"eve": eveID}
	a.Inject(parse(t, groupUpdate), func(chat.Incoming) {})
	if got := a.knownNames(groupChat(group)); len(got) != 0 {
		t.Fatalf("names survived the group change: %v", got)
	}
	if got := a.knownNames(groupChat("other")); got["eve"] != eveID {
		t.Fatalf("another chat's names were dropped: %v", got)
	}
}

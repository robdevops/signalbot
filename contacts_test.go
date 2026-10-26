package main

import (
	"bufio"
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

const (
	danID = "99999999-8888-7777-6666-555555555555"
	eveID = "55555555-4444-3333-2222-111111111111"
)

// groupUpdate is the event signal-cli sends when a group's members, name or
// description change.
const groupUpdate = `{"envelope":{"source":"` + rob + `","sourceUuid":"` + rob + `","sourceName":"Rob",` +
	`"dataMessage":{"groupInfo":{"groupId":"` + group + `","groupName":"g","revision":9,"type":"UPDATE"}}},"account":"` + self + `"}`

// The contact list is loaded when the bot connects and again whenever a group
// changes, and each load replaces the cache: a member who left is forgotten and
// a new one is known before they say anything.
func TestContactsReloadOnConnectAndGroupChange(t *testing.T) {
	var mu sync.Mutex
	calls, members := 0, `[{"uuid":"`+danID+`","givenName":"Dan"}]`
	conns := make(chan net.Conn, 1)
	sock := fakeSignal(t, func(_ int, c net.Conn, r *bufio.Reader) {
		conns <- c
		rpc(c, r, func(m string) (string, bool) {
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
	sig := NewSignal(sock)
	b := NewBot(context.Background(), testCfg(), sig, nil)
	b.names.minRefresh = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sig.Run(ctx, b.Handle)
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

	wait("the load on connect", func() bool { return b.names.lookup(danID).name == "Dan" })

	// Dan leaves and Eve joins; the group reports an update.
	mu.Lock()
	members = `[{"uuid":"` + eveID + `","givenName":"Eve"}]`
	mu.Unlock()
	for range 5 { // a burst of changes
		c.Write([]byte(receive(groupUpdate)))
	}
	wait("the reload after the group change", func() bool { return b.names.lookup(eveID).name == "Eve" })
	if got := b.names.lookup(danID).name; got != "" {
		t.Fatalf("a member who left is still known as %q", got)
	}
	time.Sleep(200 * time.Millisecond)
	if n := count(); n != 2 {
		t.Fatalf("%d contact loads, want 2 (connect, then one for the whole burst)", n)
	}

	// An ordinary message that is not for the bot changes nothing.
	c.Write([]byte(receive(groupPlain)))
	time.Sleep(200 * time.Millisecond)
	if n := count(); n != 2 {
		t.Fatalf("an unaddressed message caused a contact load: %d loads", n)
	}
}

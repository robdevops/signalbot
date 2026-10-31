package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/transports/signal"
)

const contactsJSON = `[{"number":null,"uuid":"` + alice + `","username":"alice.user20","name":"","givenName":null,"familyName":null,"nickName":null,"isArchived":false,"profile":{"lastUpdateTimestamp":1792622246207,"givenName":"Alice","familyName":null,"about":"","hasAvatar":true}}]`

func TestContactDisplayName(t *testing.T) {
	var cs []signal.Contact
	if err := json.Unmarshal([]byte(contactsJSON), &cs); err != nil {
		t.Fatal(err)
	}
	if got, rank := displayName(cs[0]); got != "Alice" || rank != rankGiven {
		t.Fatalf("profile givenName: %q %d", got, rank)
	}
	c := signal.Contact{GivenName: "Albert", Username: "alice.1"}
	c.Profile.GivenName = "Alice"
	if got, _ := displayName(c); got != "Albert" {
		t.Fatalf("contact givenName should win, got %q", got)
	}
	if got, rank := displayName((signal.Contact{Username: "alice.1"})); got != "alice.1" || rank != rankUsername {
		t.Fatalf("username fallback: %q %d", got, rank)
	}
	if got, rank := displayName((signal.Contact{})); got != "" || rank != 0 {
		t.Fatalf("empty: %q %d", got, rank)
	}
}

func TestResolvePriority(t *testing.T) {
	n := NewNames()
	m := signal.Mention{Name: alice, UUID: alice}
	if got := n.Resolve(m); got != "" {
		t.Fatalf("unknown id should resolve to nothing, not leak: %q", got)
	}
	if got := n.Resolve(signal.Mention{Name: "FromMention", UUID: alice}); got != "FromMention" {
		t.Fatalf("mention name: %q", got)
	}
	// username is the last resort, below the mention's own name
	n.replaceContacts([]signal.Contact{{UUID: alice, Username: "alice.user20"}})
	if got := n.Resolve(m); got != "alice.user20" {
		t.Fatalf("username fallback: %q", got)
	}
	if got := n.Resolve(signal.Mention{Name: "FromMention", UUID: alice}); got != "FromMention" {
		t.Fatalf("mention name beats username: %q", got)
	}
	n.Learn(&signal.Envelope{SourceUUID: alice, SourceName: "Alice Smith"})
	if got := n.Resolve(m); got != "Alice Smith" {
		t.Fatalf("sender name beats username: %q", got)
	}
	var cs []signal.Contact
	json.Unmarshal([]byte(contactsJSON), &cs)
	n.replaceContacts(cs)
	if got := n.Resolve(m); got != "Alice" {
		t.Fatalf("givenName should win: %q", got)
	}
	n.Learn(&signal.Envelope{SourceUUID: alice, SourceName: "Aimee S"})
	if got := n.Resolve(m); got != "Alice" {
		t.Fatalf("live name must not displace givenName: %q", got)
	}
}

// "@bot say hi to @Alice" where the mention carries only an id: the prompt
// sent to the model must contain the real name from listContacts.
func TestMentionNameResolvedFromContacts(t *testing.T) {
	bodies := make(chan string, 4)
	gotBadParams := make(chan struct{}, 1)
	sent := make(chan map[string]any, 2)
	ai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies <- string(b)
		w.Write([]byte(`{"choices":[{"message":{"content":"hello @alice!"}}]}`))
	}))
	defer ai.Close()

	other := "11111111-2222-3333-4444-555555555555"
	msg := `{"envelope":{"source":"` + other + `","sourceUuid":"` + other + `","sourceName":"Bob","dataMessage":{"message":"￼ say hi to ￼","mentions":[` +
		`{"name":"` + self + `","uuid":"` + self + `","start":0,"length":1},` +
		`{"name":"` + alice + `","uuid":"` + alice + `","start":12,"length":1}],"groupInfo":{"groupId":"` + group + `"}}},"account":"` + self + `"}`

	sock := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		r := bufio.NewReader(c)
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var req map[string]any
			json.Unmarshal(line, &req)
			id, _ := json.Marshal(req["id"])
			switch req["method"] {
			case "listAccounts":
				c.Write([]byte(`{"jsonrpc":"2.0","result":[{"number":null,"aci":"` + self + `"}],"id":` + string(id) + "}\n"))
				c.Write([]byte(receive(msg)))
			case "send":
				sent <- req["params"].(map[string]any)
			case "listContacts":
				if p, _ := req["params"].(map[string]any); p["allRecipients"] != true {
					gotBadParams <- struct{}{}
				}
				c.Write([]byte(`{"jsonrpc":"2.0","result":` + contactsJSON + `,"id":` + string(id) + "}\n"))
			}
		}
	}()

	cfg := &Config{Socket: sock, APIKey: "k", Model: "m", AllowedSenders: map[string]bool{}, MaxConcurrency: 4, HistoryTurns: 2}
	sig := signal.New(sock)
	x := NewOpenRouter(cfg, nil)
	x.url = ai.URL
	bot := NewBot(context.Background(), cfg, sig, x)
	ctx, cancel := contextWithCancel()
	defer cancel()
	go sig.Run(ctx, bot.Handle)

	select {
	case b := <-bodies:
		if !strings.Contains(b, "say hi to @Alice") || strings.Contains(b, alice) {
			t.Fatalf("name not resolved in request: %s", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no model request")
	}
	select {
	case p := <-sent:
		ms, _ := p["mention"].([]any)
		if len(ms) != 1 || ms[0] != "6:6:"+alice {
			t.Fatalf("want mention 6:6:%s, got %v (message %q)", alice, p["mention"], p["message"])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no send")
	}
	select {
	case <-gotBadParams:
		t.Fatal("listContacts must be called with allRecipients:true")
	default:
	}
}

func TestUnnamedMentionNotAtPrefixed(t *testing.T) {
	m := signal.Mention{UUID: "u-1", Name: "u-1", Start: 3, Length: 1}
	got := stripMentions("hi \uFFFC!", []signal.Mention{m}, self, NewNames())
	if got != "hi [unnamed user]!" {
		t.Fatalf("got %q", got)
	}
}

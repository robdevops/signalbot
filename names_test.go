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
)

const contactsJSON = `[{"number":null,"uuid":"` + rob + `","username":"robnocake.20","name":"","givenName":null,"familyName":null,"nickName":null,"isArchived":false,"profile":{"lastUpdateTimestamp":1792622246207,"givenName":"Rob","familyName":null,"about":"","hasAvatar":true}}]`

func TestContactDisplayName(t *testing.T) {
	var cs []Contact
	if err := json.Unmarshal([]byte(contactsJSON), &cs); err != nil {
		t.Fatal(err)
	}
	if got, rank := cs[0].DisplayName(); got != "Rob" || rank != rankGiven {
		t.Fatalf("profile givenName: %q %d", got, rank)
	}
	c := Contact{GivenName: "Robert", Username: "rob.1"}
	c.Profile.GivenName = "Rob"
	if got, _ := c.DisplayName(); got != "Robert" {
		t.Fatalf("contact givenName should win, got %q", got)
	}
	if got, rank := (Contact{Username: "rob.1"}).DisplayName(); got != "rob.1" || rank != rankUsername {
		t.Fatalf("username fallback: %q %d", got, rank)
	}
	if got, rank := (Contact{}).DisplayName(); got != "" || rank != 0 {
		t.Fatalf("empty: %q %d", got, rank)
	}
}

func TestResolvePriority(t *testing.T) {
	n := NewNames()
	m := Mention{Name: rob, UUID: rob}
	if got := n.Resolve(m); got != "" {
		t.Fatalf("unknown id should resolve to nothing, not leak: %q", got)
	}
	if got := n.Resolve(Mention{Name: "FromMention", UUID: rob}); got != "FromMention" {
		t.Fatalf("mention name: %q", got)
	}
	// username is the last resort, below the mention's own name
	n.addContacts([]Contact{{UUID: rob, Username: "robnocake.20"}})
	if got := n.Resolve(m); got != "robnocake.20" {
		t.Fatalf("username fallback: %q", got)
	}
	if got := n.Resolve(Mention{Name: "FromMention", UUID: rob}); got != "FromMention" {
		t.Fatalf("mention name beats username: %q", got)
	}
	n.Learn(&Envelope{SourceUUID: rob, SourceName: "Rob Stevens"})
	if got := n.Resolve(m); got != "Rob Stevens" {
		t.Fatalf("sender name beats username: %q", got)
	}
	var cs []Contact
	json.Unmarshal([]byte(contactsJSON), &cs)
	n.addContacts(cs)
	if got := n.Resolve(m); got != "Rob" {
		t.Fatalf("givenName should win: %q", got)
	}
	n.Learn(&Envelope{SourceUUID: rob, SourceName: "Robbie S"})
	if got := n.Resolve(m); got != "Rob" {
		t.Fatalf("live name must not displace givenName: %q", got)
	}
}

// "@bot say hi to @Rob" where the mention carries only an id: the prompt
// sent to x.ai must contain the real name from listContacts.
func TestMentionNameResolvedFromContacts(t *testing.T) {
	bodies := make(chan string, 4)
	gotBadParams := make(chan struct{}, 1)
	sent := make(chan map[string]any, 2)
	ai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies <- string(b)
		w.Write([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"hello @rob!"}]}]}`))
	}))
	defer ai.Close()

	other := "11111111-2222-3333-4444-555555555555"
	msg := `{"envelope":{"source":"` + other + `","sourceUuid":"` + other + `","sourceName":"Alice","dataMessage":{"message":"￼ say hi to ￼","mentions":[` +
		`{"name":"` + self + `","uuid":"` + self + `","start":0,"length":1},` +
		`{"name":"` + rob + `","uuid":"` + rob + `","start":12,"length":1}],"groupInfo":{"groupId":"` + group + `"}}},"account":"` + self + `"}`

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
	sig := NewSignal(sock)
	x := NewXAI(cfg)
	x.url = ai.URL
	bot := NewBot(context.Background(), cfg, sig, x)
	ctx, cancel := contextWithCancel()
	defer cancel()
	go sig.Run(ctx, bot.Handle)

	select {
	case b := <-bodies:
		if !strings.Contains(b, "say hi to @Rob") || strings.Contains(b, rob) {
			t.Fatalf("name not resolved in request: %s", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no x.ai request")
	}
	select {
	case p := <-sent:
		ms, _ := p["mention"].([]any)
		if len(ms) != 1 || ms[0] != "6:4:"+rob {
			t.Fatalf("want mention 6:4:%s, got %v (message %q)", rob, p["mention"], p["message"])
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
	m := Mention{UUID: "u-1", Name: "u-1", Start: 3, Length: 1}
	got := stripMentions("hi \uFFFC!", []Mention{m}, self, NewNames())
	if got != "hi [unnamed user]!" {
		t.Fatalf("got %q", got)
	}
}

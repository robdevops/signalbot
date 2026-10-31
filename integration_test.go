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
	sig := signal.NewAdapter(sock, cfg.AllowedSenders)
	x := NewOpenRouter(cfg, nil)
	x.url = ai.URL
	bot := NewBot(context.Background(), cfg, x, sig)
	ctx, cancel := contextWithCancel()
	defer cancel()
	go bot.Run(ctx)

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

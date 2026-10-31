package main

import (
	"bufio"
	"context"
	"encoding/json"
	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/llm"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/kit/logx"

	"github.com/robdevops/signalbot/internal/kit/logx/logxtest"

	"github.com/robdevops/signalbot/internal/transports/signal"
)

const (
	self  = "c7e2a9d4-1b3f-4a86-8e05-6f9d2b4a7c31"
	alice = "3b1f6c2e-7a4d-4e58-9c13-0d2a5b8e71f4"
	group = "dGVzdC1ncm91cC1pZC1maXh0dXJlLTAwMDAwMDAwMDA="
)

func parse(t *testing.T, s string) *signal.Envelope {
	t.Helper()
	var m struct{ Envelope *signal.Envelope }
	if err := json.Unmarshal([]byte(s), &m); err != nil || m.Envelope == nil {
		t.Fatalf("bad fixture: %v", err)
	}
	return m.Envelope
}

const (
	typing       = `{"envelope":{"source":"` + alice + `","sourceUuid":"` + alice + `","sourceName":"Alice","timestamp":1,"typingMessage":{"action":"STARTED","timestamp":1,"groupId":"` + group + `"}},"account":"` + self + `"}`
	groupPlain   = `{"envelope":{"source":"` + alice + `","sourceUuid":"` + alice + `","sourceName":"Alice","dataMessage":{"message":"Group test","groupInfo":{"groupId":"` + group + `","type":"DELIVER"}}},"account":"` + self + `"}`
	groupMention = `{"envelope":{"source":"` + alice + `","sourceUuid":"` + alice + `","sourceName":"Alice","dataMessage":{"message":"￼ what is up","mentions":[{"name":"` + self + `","number":null,"uuid":"` + self + `","start":0,"length":1}],"groupInfo":{"groupId":"` + group + `"}}},"account":"` + self + `"}`
	groupReply   = `{"envelope":{"source":"` + alice + `","sourceUuid":"` + alice + `","sourceName":"Alice","dataMessage":{"message":"Are you still there","quote":{"id":1,"author":"` + self + `","authorNumber":null,"authorUuid":"` + self + `","text":"oil around 78"},"groupInfo":{"groupId":"` + group + `"}}},"account":"` + self + `"}`
	dm           = `{"envelope":{"source":"` + alice + `","sourceUuid":"` + alice + `","sourceName":"Alice","dataMessage":{"message":"Test DM"}},"account":"` + self + `"}`
)

func TestDecide(t *testing.T) {
	allowed := map[string]bool{alice: true}
	tests := []struct {
		name    string
		in      string
		allowed map[string]bool
		want    string // expected prompt substring, "" = silent
		kind    string
		group   bool
	}{
		{"typing ignored", typing, allowed, "", "", false},
		{"plain group msg ignored", groupPlain, allowed, "", "", false},
		{"mention", groupMention, nil, "Alice: what is up", "mention", true},
		{"reply to bot", groupReply, nil, "Are you still there", "reply", true},
		{"reply to someone else", replyToOther, nil, "", "", false},
		{"allowed DM", dm, allowed, "Test DM", "dm", false},
		{"disallowed DM", dm, map[string]bool{"other": true}, "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := decide(parse(t, tc.in), tc.allowed)
			if tc.want == "" {
				if r != nil {
					t.Fatalf("expected silence, got %+v", r)
				}
				return
			}
			if r == nil || !strings.Contains(r.Prompt, tc.want) {
				t.Fatalf("got %+v, want prompt containing %q", r, tc.want)
			}
			if r.Kind != tc.kind {
				t.Fatalf("kind = %q, want %q", r.Kind, tc.kind)
			}
			if tc.group && r.Chat != chat.ChatID("signal:g:"+group) || !tc.group && r.Chat != chat.ChatID("signal:u:"+alice) {
				t.Fatalf("wrong chat %v", r.Chat)
			}
		})
	}
	if r := decide(parse(t, groupReply), nil); !strings.Contains(r.Prompt, "oil around 78") {
		t.Fatalf("quote context missing: %q", r.Prompt)
	}
}

func TestEndToEnd(t *testing.T) {
	ai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(401)
			return
		}
		time.Sleep(250 * time.Millisecond)
		w.Write([]byte(`{"choices":[{"message":{"content":"hi there"}}]}`))
	}))
	defer ai.Close()

	sock := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	sent := make(chan map[string]any, 10)
	events := make(chan string, 50)
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
			switch req["method"] {
			case "listAccounts":
				id, _ := json.Marshal(req["id"])
				c.Write([]byte(`{"jsonrpc":"2.0","result":[{"number":null,"aci":"` + self + `"}],"id":` + string(id) + "}\n"))
				for _, m := range []string{typing, groupPlain, groupMention, dm} {
					c.Write([]byte(receive(m)))
				}
			case "listContacts":
				id, _ := json.Marshal(req["id"])
				c.Write([]byte(`{"jsonrpc":"2.0","result":[],"id":` + string(id) + "}\n"))
			case "sendTyping":
				events <- "typing"
			case "send":
				events <- "send"
				sent <- req["params"].(map[string]any)
			}
		}
	}()

	cfg := testCfg()
	cfg.Search = true
	sig := signal.NewAdapter(sock, cfg.AllowedSenders)
	x := testModel(ai.URL)
	bot := NewBot(context.Background(), cfg, x, sig)
	ctx, cancel := contextWithCancel()
	defer cancel()
	go bot.Run(ctx)

	select {
	case p := <-sent:
		if p["groupId"] != group || p["message"] != "hi there" {
			t.Fatalf("unexpected send %v", p)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no reply sent for mention")
	}
	select {
	case p := <-sent:
		t.Fatalf("unexpected extra send (DM not allowed): %v", p)
	case <-time.After(300 * time.Millisecond):
	}
	close(events)
	var seq []string
	for e := range events {
		seq = append(seq, e)
	}
	typings := 0
	for i, e := range seq {
		if e == "typing" {
			typings++
			if i > 0 && seq[i-1] == "send" {
				t.Fatalf("typing after send: %v", seq)
			}
		}
	}
	if len(seq) == 0 || seq[0] != "typing" || seq[len(seq)-1] != "send" || typings < 2 {
		t.Fatalf("want typing x>=2 then one send, got %v", seq)
	}
}

func contextWithCancel() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

// receive wraps a fixture envelope as the JSON-RPC notification signal-cli sends.
func receive(envelope string) string {
	return `{"jsonrpc":"2.0","method":"receive","params":` + envelope + "}\n"
}

var replyToOther = strings.NewReplacer(`"author":"`+self+`"`, `"author":"x"`, `"authorUuid":"`+self+`"`, `"authorUuid":"x"`).Replace(groupReply)

// logged returns what f writes to the log.
func logged(t *testing.T, timestamps bool, f func()) string {
	t.Helper()
	buf := logxtest.Capture(t)
	if timestamps {
		log.SetFlags(log.LstdFlags)
	}
	f()
	return buf.String()
}

func TestMessageLogLine(t *testing.T) {
	r := decide(parse(t, strings.Replace(groupMention, "what is up", "what\\nis up", 1)), nil)
	if got, want := r.logLine(), "mention from Alice("+alice+")"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// Nothing logged can forge extra log records.
	m := r.Message
	m.Sender = chat.Person{ID: "Bob\n\nforged record"}
	m.Text = "hi\nalso forged"
	_, line := bot.UserTurn(m)
	if out := logged(t, false, func() { logx.Infof("%s", line) }); strings.Count(out, "\n") != 1 {
		t.Fatalf("a value split the log line: %q", out)
	}
}

func TestLogTimestampOnlyWhenInteractive(t *testing.T) {
	stamp := regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} starting\n$`)
	if out := logged(t, true, func() { logx.Infof("starting") }); !stamp.MatchString(out) {
		t.Fatalf("interactive: want a timestamp, got %q", out)
	}
	if out := logged(t, false, func() { logx.Infof("starting") }); out != "starting\n" {
		t.Fatalf("non-interactive: want no timestamp, got %q", out)
	}
}

// okAI answers every request, so tests can follow a message all the way
// through.
type okAI struct{}

func (okAI) Complete(context.Context, llm.Request) (llm.Completion, error) {
	return llm.Completion{Text: "ok"}, nil
}

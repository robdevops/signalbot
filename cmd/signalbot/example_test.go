package main

import (
	"bufio"
	"context"
	"encoding/json"
	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/llm"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/transports/signal"

	"github.com/robdevops/signalbot/internal/transports/signal/signaltest"
)

// failAI fails the test if the model is asked anything.
type failAI struct{ t *testing.T }

func (f failAI) Complete(context.Context, llm.Request) (llm.Completion, error) {
	f.t.Error("the model was called")
	return llm.Completion{Text: "x"}, nil
}

// "/example" is answered without the model, with a real mention of the sender in
// a group, and leaves no history.
func TestExample(t *testing.T) {
	sent := make(chan map[string]any, 2)
	sock := signaltest.Socket(t, func(_ int, c net.Conn, r *bufio.Reader) {
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
			case "listContacts":
				c.Write([]byte(`{"jsonrpc":"2.0","result":[{"uuid":"` + self + `","givenName":"Botty"}],"id":` + string(id) + "}\n"))
			case "send":
				sent <- req["params"].(map[string]any)
			}
		}
	})
	cfg := testCfg()
	cfg.Model, cfg.Search = "x/test-model", true
	cfg.MCPConfig = mcpServers(t, map[string][]string{"weather": {"get_forecast"}}, nil)
	sig := signal.NewAdapter(sock, cfg.AllowedSenders)
	b := NewBot(context.Background(), cfg, failAI{t}, sig)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)
	sig.Client().WaitSelf(5 * time.Second)

	recv := func() map[string]any {
		t.Helper()
		select {
		case p := <-sent:
			return p
		case <-time.After(5 * time.Second):
			t.Fatal("no reply")
			return nil
		}
	}
	deliver(b, parse(t, strings.Replace(groupMention, "what is up", "/example", 1)))
	p := recv()
	msg := p["message"].(string)
	if !strings.HasPrefix(msg, "Hi @Alice! I'm Botty.") || !strings.Contains(msg, "Run test-model with web search, charts, get_forecast") {
		t.Fatalf("message %q", msg)
	}
	if ms, _ := p["mention"].([]any); len(ms) != 1 || ms[0] != "3:6:"+alice {
		t.Fatalf("mention %v, want 3:6:%s", p["mention"], alice)
	}
	if h := b.History(chat.ChatID("signal:g:" + group)); len(h) != 0 {
		t.Fatalf("/example left history: %v", h)
	}

	// In a DM there is no one to mention.
	cfg.AllowedSenders[alice] = true
	deliver(b, parse(t, strings.Replace(dm, "Test DM", "/EXAMPLE", 1)))
	if msg := recv()["message"].(string); !strings.HasPrefix(msg, "Hi! I'm Botty.") {
		t.Fatalf("DM message %q", msg)
	}
}

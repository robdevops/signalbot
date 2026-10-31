package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/kit/logx/logxtest"

	"github.com/robdevops/signalbot/internal/transports/signal"

	"github.com/robdevops/signalbot/internal/transports/signal/signaltest"
)

func TestHandleBoundsGoroutines(t *testing.T) {
	block := make(chan struct{})
	ai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer ai.Close()

	cfg := testCfg()
	sig := signal.NewAdapter("unused", cfg.AllowedSenders)
	sig.Client().SetSelf(self)
	x := testModel(ai.URL)
	b := NewBot(context.Background(), cfg, x, sig)

	before := runtime.NumGoroutine()
	for i := 0; i < 500; i++ {
		deliver(b, parse(t, strings.Replace(groupMention, "what is up", fmt.Sprintf("msg %d", i), 1)))
	}
	time.Sleep(200 * time.Millisecond)
	if delta := runtime.NumGoroutine() - before; delta > 60 {
		t.Fatalf("500 messages created %d goroutines; the semaphore must bound them (limit %d)", delta, cfg.MaxConcurrency)
	}
	// Let the replies in flight finish, so their log lines don't land in the
	// next test's.
	close(block)
	b.Shutdown(5 * time.Second)
}

func TestCloseStopsNewReplies(t *testing.T) {
	ai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer ai.Close()
	cfg := testCfg()
	sig := signal.NewAdapter("x", cfg.AllowedSenders)
	sig.Client().SetSelf(self)
	b := NewBot(context.Background(), cfg, panicAI{}, sig) // a reply would panic, and log that

	b.Shutdown(time.Second) // nothing in flight: returns at once, holding every slot
	logs := logxtest.Capture(t)
	deliver(b, parse(t, groupMention))
	time.Sleep(50 * time.Millisecond)
	if logs.Len() != 0 {
		t.Fatalf("a message during shutdown should be dropped quietly, not logged as over capacity: %q", logs)
	}
}

func TestShutdownDrainsReplyBeforeClosingSocket(t *testing.T) {
	started := make(chan struct{}, 1)
	ai := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		time.Sleep(300 * time.Millisecond)
		w.Write([]byte(`{"choices":[{"message":{"content":"done"}}]}`))
	}))
	defer ai.Close()

	sent := make(chan string, 1)
	sock := signaltest.Socket(t, func(_ int, c net.Conn, r *bufio.Reader) {
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var req struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params map[string]any  `json:"params"`
			}
			json.Unmarshal(line, &req)
			switch req.Method {
			case "listAccounts":
				fmt.Fprintf(c, `{"jsonrpc":"2.0","result":[{"number":null,"aci":"%s"}],"id":%s}`+"\n", self, req.ID)
				fmt.Fprint(c, receive(groupMention))
			case "listContacts":
				fmt.Fprintf(c, `{"jsonrpc":"2.0","result":[],"id":%s}`+"\n", req.ID)
			case "send":
				sent <- fmt.Sprint(req.Params["message"])
			}
		}
	})

	cfg := testCfg()
	sig := signal.NewAdapter(sock, cfg.AllowedSenders)
	x := testModel(ai.URL)
	connCtx, closeConn := context.WithCancel(context.Background())
	defer closeConn()
	b := NewBot(connCtx, cfg, x, sig)
	go b.Run(connCtx)

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("model call never started")
	}
	// The shutdown sequence main runs on SIGTERM, with the reply mid-flight.
	b.Shutdown(5 * time.Second)
	closeConn()

	select {
	case msg := <-sent:
		if msg != "done" {
			t.Fatalf("sent %q", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a reply in flight at shutdown was lost")
	}
}

package signal

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/transports/signal/signaltest"
)

func TestOversizedFrameDropsConnectionAndReconnects(t *testing.T) {
	conns := make(chan int, 4)
	sock := signaltest.Socket(t, func(n int, c net.Conn, r *bufio.Reader) {
		conns <- n
		if n == 1 {
			go io.Copy(io.Discard, r)
			c.Write(append(bytes.Repeat([]byte("x"), maxLine+10), '\n'))
			return
		}
		signaltest.RPC(c, r, func(m string) (string, bool) {
			return `[{"number":null,"aci":"self"}]`, m == "listAccounts"
		})
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go New(sock).Run(ctx, func(*Envelope) {})
	for want := 1; want <= 2; want++ {
		select {
		case n := <-conns:
			if n != want {
				t.Fatalf("got connection %d, want %d", n, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no connection %d: an oversized frame should drop the connection, then reconnect", want)
		}
	}
}

func TestSocketNetwork(t *testing.T) {
	for addr, want := range map[string]string{
		"/run/signal-cli/socket": "unix", "run/signal-cli/socket": "unix", "./s": "unix",
		"@abstract": "unix", "socket": "unix",
		"127.0.0.1:7583": "tcp", "localhost:7583": "tcp", "[::1]:7583": "tcp",
	} {
		if got := New(addr).network(); got != want {
			t.Errorf("%q: got %s, want %s", addr, got, want)
		}
	}
}

func TestReconnectsDoNotLeakGoroutines(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := New("x")
	before := runtime.NumGoroutine()
	for i := 0; i < 50; i++ { // 50 connections that drop at once, as signal-cli restarting would
		c1, c2 := net.Pipe()
		c2.Close()
		s.serve(ctx, c1, func(*Envelope) {})
	}
	time.Sleep(100 * time.Millisecond)
	if delta := runtime.NumGoroutine() - before; delta > 5 {
		t.Fatalf("%d goroutines left behind by 50 dropped connections", delta)
	}
}

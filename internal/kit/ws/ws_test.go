package ws_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/kit/httpx"
	"github.com/robdevops/signalbot/internal/kit/ws"
	"github.com/robdevops/signalbot/internal/kit/ws/wstest"
)

// serve runs handle on each WebSocket a test dials, and returns the URL.
func serve(t *testing.T, handle func(c *wstest.Conn, r *http.Request)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := wstest.Accept(w, r)
		if err != nil {
			return
		}
		handle(c, r)
	}))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

func dial(t *testing.T, u string, limit int64) *ws.Conn {
	t.Helper()
	c, err := ws.Dial(context.Background(), http.DefaultClient, u, http.Header{"Authorization": {"Bot x"}}, limit)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close(1000, "") })
	return c
}

// A text message each way; the client masks its frames, and sends the
// handshake's header.
func TestEcho(t *testing.T) {
	got := make(chan wstest.Frame, 1)
	u := serve(t, func(c *wstest.Conn, r *http.Request) {
		if r.Header.Get("Authorization") != "Bot x" {
			c.CloseWith(4000, "no header")
			return
		}
		f, err := c.ReadFrame()
		if err != nil {
			return
		}
		got <- f
		c.Write("pong: " + string(f.Payload))
		c.Read() // until closed
	})
	c := dial(t, u, 1<<20)
	ctx := context.Background()
	if err := c.Write(ctx, true, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	if f := <-got; !f.Masked || !f.Fin || f.Op != wstest.Text || string(f.Payload) != "hello" {
		t.Fatalf("frame %+v", f)
	}
	text, msg, err := c.Read(ctx)
	if err != nil || !text || string(msg) != "pong: hello" {
		t.Fatalf("%v %q %v", text, msg, err)
	}
}

// Large messages use the 16- and 64-bit lengths both ways.
func TestLengths(t *testing.T) {
	sizes := []int{125, 126, 65535, 65536, 200000}
	u := serve(t, func(c *wstest.Conn, _ *http.Request) {
		for range sizes {
			m, err := c.Read()
			if err != nil {
				return
			}
			c.Write(m)
		}
		c.Read()
	})
	c := dial(t, u, 1<<20)
	ctx := context.Background()
	for _, n := range sizes {
		c.Write(ctx, true, []byte(strings.Repeat("a", n)))
		if _, msg, err := c.Read(ctx); err != nil || len(msg) != n {
			t.Fatalf("%d: %d %v", n, len(msg), err)
		}
	}
}

// A fragmented message is reassembled, with a ping between its fragments
// answered by a pong.
func TestFragmentsAndPing(t *testing.T) {
	pong := make(chan wstest.Frame, 1)
	u := serve(t, func(c *wstest.Conn, _ *http.Request) {
		c.WriteFrame(false, wstest.Text, []byte("hel"))
		c.WriteFrame(true, wstest.Ping, []byte("beat"))
		c.WriteFrame(false, wstest.Continuation, []byte("lo "))
		c.WriteFrame(true, wstest.Continuation, []byte("world"))
		f, err := c.ReadFrame()
		if err == nil {
			pong <- f
		}
		c.Read()
	})
	c := dial(t, u, 1<<20)
	text, msg, err := c.Read(context.Background())
	if err != nil || !text || string(msg) != "hello world" {
		t.Fatalf("%v %q %v", text, msg, err)
	}
	if f := <-pong; f.Op != wstest.Pong || string(f.Payload) != "beat" || !f.Masked {
		t.Fatalf("pong %+v", f)
	}
}

// The peer's close is a CloseError with its code and reason, answered.
func TestPeerClose(t *testing.T) {
	answered := make(chan wstest.Frame, 1)
	u := serve(t, func(c *wstest.Conn, _ *http.Request) {
		c.WriteFrame(true, wstest.Close, append([]byte{0x0F, 0xA4}, "session ended"...)) // 4004
		if f, err := c.ReadFrame(); err == nil {
			answered <- f
		}
	})
	c := dial(t, u, 1<<20)
	_, _, err := c.Read(context.Background())
	var ce *ws.CloseError
	if !errors.As(err, &ce) || ce.Code != 4004 || ce.Reason != "session ended" {
		t.Fatalf("err %v", err)
	}
	if f := <-answered; f.Op != wstest.Close {
		t.Fatalf("answer %+v", f)
	}
	if err := c.Write(context.Background(), true, []byte("late")); err == nil {
		t.Error("wrote after the close")
	}
}

// A message past the limit, in one frame or across fragments, is refused,
// and the connection closed with 1009.
func TestLimit(t *testing.T) {
	for _, frags := range []int{1, 3} {
		closed := make(chan []byte, 1)
		u := serve(t, func(c *wstest.Conn, _ *http.Request) {
			if frags == 1 {
				c.WriteFrame(true, wstest.Text, []byte(strings.Repeat("x", 101)))
			} else {
				c.WriteFrame(false, wstest.Text, []byte(strings.Repeat("x", 50)))
				c.WriteFrame(false, wstest.Continuation, []byte(strings.Repeat("x", 40)))
				c.WriteFrame(true, wstest.Continuation, []byte(strings.Repeat("x", 40)))
			}
			if f, err := c.ReadFrame(); err == nil && f.Op == wstest.Close {
				closed <- f.Payload
			}
		})
		c := dial(t, u, 100)
		if _, _, err := c.Read(context.Background()); !errors.Is(err, ws.ErrTooLarge) {
			t.Fatalf("%d fragments: %v", frags, err)
		}
		if p := <-closed; len(p) < 2 || p[0] != 0x03 || p[1] != 0xF1 { // 1009
			t.Fatalf("%d fragments: close %v", frags, p)
		}
	}
}

// A dropped connection is an error; a masked server frame or a stray
// continuation is a protocol error.
func TestBroken(t *testing.T) {
	for name, send := range map[string]func(c *wstest.Conn){
		"dropped":      func(c *wstest.Conn) { c.Drop() },
		"continuation": func(c *wstest.Conn) { c.WriteFrame(true, wstest.Continuation, []byte("x")) },
		"long ping":    func(c *wstest.Conn) { c.WriteFrame(true, wstest.Ping, []byte(strings.Repeat("p", 126))) },
	} {
		u := serve(t, func(c *wstest.Conn, _ *http.Request) { send(c); c.Read() })
		c := dial(t, u, 1<<20)
		if _, _, err := c.Read(context.Background()); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// Read returns when its context ends, and the connection is closed.
func TestCancel(t *testing.T) {
	u := serve(t, func(c *wstest.Conn, _ *http.Request) { c.Read() })
	c := dial(t, u, 1<<20)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := c.Read(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
}

// A handshake answered without a 101, or with the wrong accept, fails.
func TestHandshake(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/wrong" {
			w.Header().Set("Upgrade", "websocket")
			w.Header().Set("Connection", "Upgrade")
			w.Header().Set("Sec-WebSocket-Accept", "nope")
			w.WriteHeader(http.StatusSwitchingProtocols)
			return
		}
		http.Error(w, "no", http.StatusUnauthorized)
	}))
	defer srv.Close()
	for _, p := range []string{"/refused", "/wrong"} {
		if _, err := ws.Dial(context.Background(), http.DefaultClient, srv.URL+p, nil, 1<<20); err == nil {
			t.Errorf("%s: dialled", p)
		}
	}
}

// Through the bot's own client, with its proxy setting: the handshake goes
// by the proxy, as Go's client sends any request.
func TestThroughProxy(t *testing.T) {
	u := serve(t, func(c *wstest.Conn, _ *http.Request) {
		if m, err := c.Read(); err == nil {
			c.Write(m)
		}
		c.Read()
	})
	proxied := make(chan string, 1)
	target, _ := url.Parse(strings.Replace(u, "ws://", "http://", 1))
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied <- r.URL.String()
		// A plain-HTTP proxy forwards the upgrade as it is.
		httputil.NewSingleHostReverseProxy(target).ServeHTTP(w, r)
	}))
	defer proxy.Close()
	pu, _ := url.Parse(proxy.URL)
	client := httpx.Client(1)
	client.Transport.(*http.Transport).Proxy = http.ProxyURL(pu)
	c, err := ws.Dial(context.Background(), client, u, nil, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(1000, "")
	if got := <-proxied; !strings.HasPrefix(got, "http://127.0.0.1") {
		t.Fatalf("proxied %q", got)
	}
	c.Write(context.Background(), true, []byte("via proxy"))
	if _, msg, err := c.Read(context.Background()); err != nil || string(msg) != "via proxy" {
		t.Fatalf("%q %v", msg, err)
	}
}

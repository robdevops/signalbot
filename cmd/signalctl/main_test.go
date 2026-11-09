package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// daemon is a fake signal-cli daemon: it sends every connection a received
// message first, as the real one does, then answers each request with
// reply(method, params), or an error if reply returns ok false.
type daemon struct {
	addr string
	got  chan map[string]any
}

const secret = "a private message nobody may see"

func newDaemon(t *testing.T, network string, reply func(method string, params json.RawMessage) (any, bool)) *daemon {
	t.Helper()
	addr := "127.0.0.1:0"
	if network == "unix" {
		addr = filepath.Join(t.TempDir(), "s.sock")
	}
	l, err := net.Listen(network, addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	d := &daemon{addr: l.Addr().String(), got: make(chan map[string]any, 10)}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.Write([]byte(`{"jsonrpc":"2.0","method":"receive","params":{"envelope":{"dataMessage":{"message":"` + secret + `"}}}}` + "\n"))
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					var req struct {
						ID     json.RawMessage
						Method string
						Params json.RawMessage
					}
					json.Unmarshal(sc.Bytes(), &req)
					var all map[string]any
					json.Unmarshal(sc.Bytes(), &all)
					d.got <- all
					if reply == nil {
						continue // never answers
					}
					res, ok := reply(req.Method, req.Params)
					var b []byte
					if ok {
						b, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res})
					} else {
						b, _ = json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -1, "message": res}})
					}
					c.Write(append(b, '\n'))
				}
			}()
		}
	}()
	return d
}

func runCmd(t *testing.T, addr string, args ...string) (code int, out, errOut string) {
	t.Helper()
	var o, e bytes.Buffer
	code = run(args, addr, &o, &e)
	if strings.Contains(o.String()+e.String(), secret) {
		t.Fatal("a received message was printed")
	}
	return code, o.String(), e.String()
}

func TestBlockSendsRecipientsAndGroups(t *testing.T) {
	for _, network := range []string{"unix", "tcp"} {
		d := newDaemon(t, network, func(string, json.RawMessage) (any, bool) { return map[string]any{}, true })
		code, out, _ := runCmd(t, d.addr, "block", "+1555", "group:Z3Jv", "3f9a")
		if code != 0 || out != "blocked +1555, group:Z3Jv, 3f9a\n" {
			t.Fatalf("%s: code %d, out %q", network, code, out)
		}
		req := <-d.got
		b, _ := json.Marshal(req["params"])
		if req["method"] != "block" || req["jsonrpc"] != "2.0" || string(b) != `{"groupId":["Z3Jv"],"recipient":["+1555","3f9a"]}` {
			t.Fatalf("%s: request %v", network, req)
		}
	}
}

func TestBlockedListsPeopleAndGroups(t *testing.T) {
	d := newDaemon(t, "unix", func(method string, params json.RawMessage) (any, bool) {
		switch method {
		case "listContacts":
			if string(params) != `{"blocked":true}` {
				return "bad params " + string(params), false
			}
			return []any{
				map[string]any{"number": "+1555", "uuid": "3f9a", "name": "Bob B", "givenName": "Bob", "isBlocked": true},
				map[string]any{"number": "+1666", "uuid": nil, "name": nil, "givenName": nil, "profile": map[string]any{"givenName": "Carol"}},
			}, true
		case "listGroups":
			return []any{map[string]any{"id": "Z3Jv", "name": "Spam", "isBlocked": true}, map[string]any{"id": "b2s=", "name": "Fine", "isBlocked": false}}, true
		}
		return "unknown method", false
	})
	code, out, errOut := runCmd(t, d.addr, "blocked")
	want := "person\t3f9a\tBob B\nperson\t+1666\tCarol\ngroup\tgroup:Z3Jv\tSpam\n"
	if code != 0 || out != want {
		t.Fatalf("code %d, out %q, err %q", code, out, errOut)
	}
}

func TestCallPassesParamsAndPrintsResult(t *testing.T) {
	d := newDaemon(t, "tcp", func(method string, params json.RawMessage) (any, bool) {
		return map[string]any{"method": method, "params": json.RawMessage(params)}, true
	})
	code, out, _ := runCmd(t, d.addr, "call", "listGroups", `{"detailed":true}`)
	if code != 0 || !strings.Contains(out, `"method": "listGroups"`) || !strings.Contains(out, `"detailed": true`) {
		t.Fatalf("code %d, out %q", code, out)
	}
	if code, _, errOut := runCmd(t, d.addr, "call", "listGroups", "{not json"); code != 1 || !strings.Contains(errOut, "params must be JSON") {
		t.Fatalf("bad params: code %d, %q", code, errOut)
	}
}

func TestErrorsAndUsage(t *testing.T) {
	d := newDaemon(t, "unix", func(string, json.RawMessage) (any, bool) { return "Invalid group id", false })
	if code, _, errOut := runCmd(t, d.addr, "unblock", "group:x"); code != 1 || errOut != "signalctl: unblock: Invalid group id\n" {
		t.Fatalf("code %d, %q", code, errOut)
	}
	for _, args := range [][]string{nil, {"block"}, {"blocked", "x"}, {"call"}, {"frobnicate"}} {
		if code, _, errOut := runCmd(t, d.addr, args...); code != 2 || !strings.HasPrefix(errOut, "usage:") {
			t.Fatalf("%v: code %d, %q", args, code, errOut)
		}
	}
	if code, _, errOut := runCmd(t, filepath.Join(t.TempDir(), "none.sock"), "blocked"); code != 1 || !strings.Contains(errOut, "can't reach signal-cli") {
		t.Fatalf("no daemon: code %d, %q", code, errOut)
	}
}

func TestNoAnswerTimesOut(t *testing.T) {
	old := timeout
	timeout = 100 * time.Millisecond
	defer func() { timeout = old }()
	d := newDaemon(t, "unix", nil)
	if code, _, errOut := runCmd(t, d.addr, "blocked"); code != 1 || !strings.Contains(errOut, "no answer from signal-cli") {
		t.Fatalf("code %d, %q", code, errOut)
	}
}

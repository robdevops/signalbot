// Package signaltest fakes a signal-cli socket for tests.
package signaltest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"testing"
)

// Socket serves a signal-cli socket; onConn handles the n-th connection.
func Socket(t *testing.T, onConn func(n int, c net.Conn, r *bufio.Reader)) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for n := 1; ; n++ {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go onConn(n, c, bufio.NewReader(c))
		}
	}()
	return sock
}

// RPC answers each request whose method has an entry in results.
func RPC(c net.Conn, r *bufio.Reader, results func(method string) (string, bool)) {
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		json.Unmarshal(line, &req)
		if res, ok := results(req.Method); ok {
			fmt.Fprintf(c, `{"jsonrpc":"2.0","result":%s,"id":%s}`+"\n", res, req.ID)
		}
	}
}

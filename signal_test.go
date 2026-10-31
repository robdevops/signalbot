package main

import (
	"bytes"
	"io"
	"log"
	"strings"
	"testing"
)

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	redirectLogs(t, &buf, &buf)
	return &buf
}

// redirectLogs sends stdout lines to out and stderr lines to errOut, without
// timestamps, until the test ends.
func redirectLogs(t *testing.T, out, errOut io.Writer) {
	t.Helper()
	w, flags := log.Writer(), log.Flags()
	ew, eflags := errLog.Writer(), errLog.Flags()
	log.SetOutput(out)
	log.SetFlags(0)
	errLog.SetOutput(errOut)
	errLog.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(w)
		log.SetFlags(flags)
		errLog.SetOutput(ew)
		errLog.SetFlags(eflags)
	})
}

func TestUnmatchedResponsesAreLogged(t *testing.T) {
	tests := []struct {
		name, line, want string
	}{
		{"rpc error", `{"jsonrpc":"2.0","error":{"code":-1,"message":"Failed to send message"},"id":7}`, "Failed to send message"},
		{"not delivered", `{"jsonrpc":"2.0","result":{"timestamp":1,"results":[{"type":"NETWORK_FAILURE"}]},"id":8}`, "NETWORK_FAILURE"},
		{"delivered is quiet", `{"jsonrpc":"2.0","result":{"timestamp":1,"results":[{"type":"SUCCESS"}]},"id":9}`, ""},
		{"typing result is quiet", `{"jsonrpc":"2.0","result":{"timestamp":1},"id":10}`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureLogs(t)
			NewSignal("x").dispatch([]byte(tc.line), func(*Envelope) { t.Fatal("unexpected envelope") })
			out := buf.String()
			if tc.want == "" && out != "" || !strings.Contains(out, tc.want) {
				t.Fatalf("log output %q, want containing %q", out, tc.want)
			}
		})
	}
}

package main

import (
	"strings"
	"testing"

	"github.com/robdevops/signalbot/internal/kit/logx/logxtest"
)

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
			buf := logxtest.Capture(t)
			NewSignal("x").dispatch([]byte(tc.line), func(*Envelope) { t.Fatal("unexpected envelope") })
			out := buf.String()
			if tc.want == "" && out != "" || !strings.Contains(out, tc.want) {
				t.Fatalf("log output %q, want containing %q", out, tc.want)
			}
		})
	}
}

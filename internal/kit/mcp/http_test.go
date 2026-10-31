package mcp

import (
	"strings"
	"testing"
)

// One event split over several data: lines is joined with newlines.
func TestReadSSEMultiLine(t *testing.T) {
	stream := "event: message\ndata: {\"jsonrpc\":\"2.0\",\ndata: \"id\":7,\"result\":{\"ok\":true}}\n\n"
	m, err := readSSE(strings.NewReader(stream), 7)
	if err != nil || string(m.Result) != `{"ok":true}` {
		t.Fatalf("got %+v, %v", m, err)
	}
}

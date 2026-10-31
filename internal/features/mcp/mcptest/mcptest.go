// Package mcptest is a fake MCP server for tests.
package mcptest

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"strings"
	"sync"
)

// protocol is the MCP version the fake speaks.
const protocol = "2025-06-18"

// Server is a Streamable HTTP MCP server with three tools: echo returns its
// arguments, fail reports an error, and pic returns an image.
type Server struct {
	SSE   bool
	Tools []string // tool names to list instead, each answering like echo

	mu       sync.Mutex
	session  string // the live session id
	inits    int
	badCalls int // requests that carried the wrong session id
}

func (f *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     *int64          `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	defer f.mu.Unlock()
	if req.Method == "initialize" {
		f.inits++
		f.session = fmt.Sprintf("s%d", f.inits)
		w.Header().Set("Mcp-Session-Id", f.session)
	} else if r.Header.Get("Mcp-Session-Id") != f.session {
		f.badCalls++
		http.Error(w, "unknown session", http.StatusNotFound)
		return
	} else if r.Header.Get("MCP-Protocol-Version") != protocol {
		http.Error(w, "no protocol version", http.StatusBadRequest)
		return
	}
	if req.ID == nil { // a notification
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result string
	switch req.Method {
	case "initialize":
		result = `{"protocolVersion":"` + protocol + `","capabilities":{"tools":{}},"serverInfo":{"name":"fake","version":"1"}}`
	case "tools/list":
		var p struct{ Cursor string }
		json.Unmarshal(req.Params, &p)
		if len(f.Tools) > 0 {
			var tools []string
			for _, n := range f.Tools {
				tools = append(tools, `{"name":"`+n+`","inputSchema":{"type":"object"}}`)
			}
			result = `{"tools":[` + strings.Join(tools, ",") + `]}`
		} else if p.Cursor == "" {
			result = `{"tools":[{"name":"echo","description":"echoes","inputSchema":{"type":"object"}}],"nextCursor":"2"}`
		} else {
			result = `{"tools":[{"name":"fail","inputSchema":{"type":"object"}},{"name":"pic"}]}`
		}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		json.Unmarshal(req.Params, &p)
		args, _ := json.Marshal(p.Arguments)
		text, _ := json.Marshal("echo " + string(args))
		switch p.Name {
		case "fail":
			result = `{"content":[{"type":"text","text":"it broke"}],"isError":true}`
		case "pic":
			result = `{"content":[{"type":"image","data":"x"},{"type":"image","mimeType":"image/png","data":"` + tinyPNG() + `"},{"type":"text","text":"caption"}]}`
		default:
			result = `{"content":[{"type":"text","text":` + string(text) + `}]}`
		}
	}
	msg := fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"result":%s}`, *req.ID, result)
	if f.SSE {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", msg)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, msg)
}

// Inits is the number of initialize requests so far.
func (f *Server) Inits() int { f.mu.Lock(); defer f.mu.Unlock(); return f.inits }

// BadCalls is the number of requests that carried the wrong session id.
func (f *Server) BadCalls() int { f.mu.Lock(); defer f.mu.Unlock(); return f.badCalls }

// Expire forgets the live session, as a server restart would.
func (f *Server) Expire() { f.mu.Lock(); defer f.mu.Unlock(); f.session = "gone" }

// TinyPNG is a 1×1 PNG as base64.
func TinyPNG() string { return tinyPNG() }

func tinyPNG() string {
	var buf bytes.Buffer
	png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1)))
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

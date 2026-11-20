package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
)

// The protocol GeoGuess speaks: MCP 2025-06-18, as newline-delimited
// JSON-RPC 2.0 on stdin and stdout, plus signalbot's addon extension.
const (
	protocolVersion = "2025-06-18"
	vendor          = "io.github.robdevops.signalbot/"
	outboxURI       = "signalbot://outbox"
)

// maxLine caps one incoming line. signalbot's requests are small: a
// command's arguments or one chat message.
const maxLine = 1 << 20

// JSON-RPC's error codes, and MCP's for a resource that isn't there.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeNoMethod       = -32601
	codeInvalidParams  = -32602
	codeNotFound       = -32002
)

// message is one JSON-RPC message read: a request, a notification (no ID)
// or a response to a request of ours (GeoGuess sends none).
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// response is the answer to one request: its result or an error.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// notification is a message to the client that wants no answer.
type notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

// wire writes whole messages to stdout, one per line, one at a time: it is
// the only writer of stdout, which carries nothing but the protocol.
type wire struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *wire) send(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	_, err = w.w.Write(append(data, '\n'))
	return err
}

// handler answers one request's params with a result, or an *rpcError.
type handler func(ctx context.Context, params json.RawMessage) (any, error)

// serve reads requests from in until it ends or ctx does, answering each on
// its own goroutine through out, and waits for the answers in flight. A
// request the client cancels (notifications/cancelled) has its context
// cancelled, so it stops rather than act for no one.
func (s *server) serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.out = &wire{w: out}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.notifier(ctx)
	var wg sync.WaitGroup
	defer wg.Wait()
	var mu sync.Mutex
	inFlight := map[string]context.CancelFunc{} // by request ID
	r := bufio.NewReaderSize(in, 64<<10)
	for {
		line, err := readLine(r)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if line == nil { // too long, or blank
			continue
		}
		var m message
		if json.Unmarshal(line, &m) != nil {
			s.out.send(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{codeParse, "parse error"}})
			continue
		}
		if m.Method == "notifications/cancelled" {
			var p struct {
				RequestID json.RawMessage `json:"requestId"`
			}
			json.Unmarshal(m.Params, &p)
			mu.Lock()
			if stop := inFlight[string(p.RequestID)]; stop != nil {
				stop()
			}
			mu.Unlock()
			continue
		}
		if len(m.ID) == 0 || m.Method == "" { // a notification, or a response
			continue
		}
		id := string(m.ID)
		rctx, stop := context.WithCancel(ctx)
		mu.Lock()
		inFlight[id] = stop
		mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.out.send(s.answer(rctx, m))
			mu.Lock()
			delete(inFlight, id)
			mu.Unlock()
			stop()
		}()
	}
}

// answer runs one request's handler.
func (s *server) answer(ctx context.Context, m message) response {
	resp := response{JSONRPC: "2.0", ID: m.ID}
	h, ok := s.methods()[m.Method]
	switch {
	case m.JSONRPC != "2.0":
		resp.Error = &rpcError{codeInvalidRequest, "not JSON-RPC 2.0"}
	case !ok:
		resp.Error = &rpcError{codeNoMethod, "method not found"}
	default:
		result, err := h(ctx, m.Params)
		var re *rpcError
		switch {
		case errors.As(err, &re):
			resp.Error = re
		case err != nil:
			resp.Error = &rpcError{-32603, "internal error"}
		default:
			resp.Result = result
		}
	}
	return resp
}

// readLine reads one line without its end. A line longer than maxLine is
// skipped whole, and nil returned for it, as for a blank one.
func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	tooLong := false
	for {
		part, err := r.ReadSlice('\n')
		if !tooLong {
			line = append(line, part...)
			if len(line) > maxLine {
				line, tooLong = nil, true
			}
		}
		switch {
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		case err != nil && (len(line) == 0 || !errors.Is(err, io.EOF)):
			return nil, err
		}
		if tooLong {
			return nil, nil
		}
		line = trimEOL(line)
		if len(line) == 0 {
			return nil, nil
		}
		return line, nil
	}
}

func trimEOL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ') {
		b = b[:len(b)-1]
	}
	return b
}

// params reads a request's params into v, or fails as JSON-RPC's invalid
// params.
func params(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	if json.Unmarshal(raw, v) != nil {
		return &rpcError{codeInvalidParams, "invalid params"}
	}
	return nil
}

// The MCP methods' results.
type (
	initializeResult struct {
		ProtocolVersion string       `json:"protocolVersion"`
		Capabilities    capabilities `json:"capabilities"`
		ServerInfo      serverInfo   `json:"serverInfo"`
	}
	capabilities struct {
		Experimental map[string]addonCapability `json:"experimental"`
		Tools        struct{}                   `json:"tools"`
		Resources    resourcesCapability        `json:"resources"`
	}
	addonCapability struct {
		Version int    `json:"version"`
		Network string `json:"network,omitempty"` // the bot's chat network, as the bot offers it
	}
	resourcesCapability struct {
		Subscribe bool `json:"subscribe"`
	}
	serverInfo struct {
		Name    string `json:"name"`
		Title   string `json:"title,omitempty"`
		Version string `json:"version"`
	}

	tool struct {
		Name        string     `json:"name"`
		Description string     `json:"description"`
		InputSchema schema     `json:"inputSchema"`
		Meta        visibility `json:"_meta"`
	}
	schema struct {
		Type       string            `json:"type"`
		Properties map[string]schema `json:"properties,omitempty"`
		Required   []string          `json:"required,omitempty"`
	}
	visibility struct {
		Visibility []string `json:"io.github.robdevops.signalbot/visibility"`
	}

	toolResult struct {
		Content []content   `json:"content"`
		IsError bool        `json:"isError,omitempty"`
		Meta    *resultMeta `json:"_meta,omitempty"`
	}
	content struct {
		Type     string `json:"type"` // text or image
		Text     string `json:"text,omitempty"`
		Data     string `json:"data,omitempty"` // base64
		MimeType string `json:"mimeType,omitempty"`
	}
	resultMeta struct {
		Watch *watch `json:"io.github.robdevops.signalbot/watch,omitempty"`
		React string `json:"io.github.robdevops.signalbot/react,omitempty"`
	}
	watch struct {
		ID      string `json:"id"`
		Pattern string `json:"pattern"`
		Until   string `json:"until"` // RFC 3339
		Tool    string `json:"tool"`
		Guard   string `json:"guard,omitempty"`
	}

	resourceTemplate struct {
		URITemplate string `json:"uriTemplate"`
		Name        string `json:"name"`
		MimeType    string `json:"mimeType"`
	}
	readResult struct {
		Contents []resourceContent `json:"contents"`
	}
	resourceContent struct {
		URI      string `json:"uri"`
		MimeType string `json:"mimeType"`
		Text     string `json:"text"`
	}
	uriParams struct {
		URI string `json:"uri"`
	}
)

// callParams are tools/call's params, with the context signalbot puts in
// _meta when the bot itself makes the call.
type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Meta      struct {
		Context *callContext `json:"io.github.robdevops.signalbot/context"`
	} `json:"_meta"`
}

// callContext is the message a call came from: pseudonyms for its chat and
// sender, and the bot's receive times in whole seconds; and, for a call the
// bot waits on, when it stops waiting.
type callContext struct {
	Chat      string `json:"chat"`
	Sender    string `json:"sender"`
	Ref       string `json:"ref"`
	Addressed bool   `json:"addressed"`
	Group     bool   `json:"group"`
	Sent      int64  `json:"sent"`
	Edited    int64  `json:"edited"`
	Deadline  int64  `json:"deadline,omitempty"`
}

// methods are the MCP methods GeoGuess answers.
func (s *server) methods() map[string]handler {
	return map[string]handler{
		"initialize": func(_ context.Context, raw json.RawMessage) (any, error) {
			var p struct {
				Capabilities struct {
					Experimental map[string]addonCapability `json:"experimental"`
				} `json:"capabilities"`
			}
			json.Unmarshal(raw, &p) // optional: without a network the caption names both
			s.mu.Lock()
			s.network = p.Capabilities.Experimental[vendor+"addon"].Network
			s.mu.Unlock()
			r := initializeResult{ProtocolVersion: protocolVersion, ServerInfo: serverInfo{Name: "geoguess", Title: "GeoGuess", Version: "1"}}
			r.Capabilities.Experimental = map[string]addonCapability{vendor + "addon": {Version: 1}}
			r.Capabilities.Resources.Subscribe = true
			return r, nil
		},
		"ping": func(context.Context, json.RawMessage) (any, error) { return struct{}{}, nil },
		"tools/list": func(context.Context, json.RawMessage) (any, error) {
			return struct {
				Tools []tool `json:"tools"`
			}{tools}, nil
		},
		"tools/call": func(ctx context.Context, raw json.RawMessage) (any, error) {
			var p callParams
			if err := params(raw, &p); err != nil {
				return nil, err
			}
			return s.call(ctx, p)
		},
		"resources/list": func(context.Context, json.RawMessage) (any, error) {
			return struct {
				Resources []struct{} `json:"resources"`
			}{[]struct{}{}}, nil
		},
		"resources/templates/list": func(context.Context, json.RawMessage) (any, error) {
			return struct {
				Templates []resourceTemplate `json:"resourceTemplates"`
			}{[]resourceTemplate{{URITemplate: outboxURI + "/{cursor}", Name: "outbox", MimeType: "application/json"}}}, nil
		},
		"resources/subscribe":   func(_ context.Context, raw json.RawMessage) (any, error) { return s.subscribe(raw, true) },
		"resources/unsubscribe": func(_ context.Context, raw json.RawMessage) (any, error) { return s.subscribe(raw, false) },
		"resources/read": func(_ context.Context, raw json.RawMessage) (any, error) {
			var p uriParams
			if err := params(raw, &p); err != nil {
				return nil, err
			}
			cursor, ok := strings.CutPrefix(p.URI, outboxURI+"/")
			if !ok {
				return nil, &rpcError{codeNotFound, "resource not found"}
			}
			page, err := json.Marshal(s.readOutbox(cursor))
			if err != nil {
				return nil, err
			}
			return readResult{[]resourceContent{{URI: p.URI, MimeType: "application/json", Text: string(page)}}}, nil
		},
	}
}

// tools are GeoGuess's tools, both for the bot alone: neither is offered to
// the model.
var tools = []tool{
	{
		Name:        "geo",
		Description: "The /geo command: start, end or skip this chat's round, or show its scores.",
		InputSchema: schema{Type: "object", Properties: map[string]schema{"args": {Type: "string"}}},
		Meta:        visibility{[]string{"bot"}},
	},
	{
		Name:        "geo_guess",
		Description: "Records the location in one chat message as a guess in this chat's round.",
		InputSchema: schema{Type: "object", Properties: map[string]schema{"text": {Type: "string"}}, Required: []string{"text"}},
		Meta:        visibility{[]string{"bot"}},
	},
}

// subscribe records whether the client wants to hear of the outbox.
func (s *server) subscribe(raw json.RawMessage, on bool) (any, error) {
	var p uriParams
	if err := params(raw, &p); err != nil {
		return nil, err
	}
	if _, ok := strings.CutPrefix(p.URI, outboxURI); !ok {
		return nil, &rpcError{codeNotFound, "resource not found"}
	}
	s.mu.Lock()
	s.subscribed = on
	pending := len(s.outbox) > 0
	s.mu.Unlock()
	if on && pending {
		s.poke()
	}
	return struct{}{}, nil
}

// notifier tells a subscribed client each time the outbox gains ops, on
// its own goroutine, as a write can wait on a client that is busy.
func (s *server) notifier(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.dirty:
		}
		s.mu.Lock()
		on := s.subscribed
		s.mu.Unlock()
		if on {
			s.out.send(notification{JSONRPC: "2.0", Method: "notifications/resources/updated", Params: uriParams{outboxURI}})
		}
	}
}

// poke marks the outbox changed, without waiting.
func (s *server) poke() {
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

package main

// The fakes behind the behaviour scenarios, and the harness API the scenarios
// use: a fake signal-cli on a unix socket, a fake OpenRouter, fake MCP
// servers and a captured log. Nothing here names the bot's code, so this file
// and scenario_test.go stay as they are when the bot is rewritten; only
// scenario_start_test.go, which wires the bot to these fakes, changes.

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"
)

// The placeholder ids the scenarios use.
const (
	idBot   = "c7e2a9d4-1b3f-4a86-8e05-6f9d2b4a7c31"
	idAlice = "3b1f6c2e-7a4d-4e58-9c13-0d2a5b8e71f4"
	idBob   = "99999999-8888-7777-6666-555555555555"
	idGroup = "dGVzdC1ncm91cC1pZC1maXh0dXJlLTAwMDAwMDAwMDA="

	// scenarioModel is the model the bot runs unless a scenario sets MODEL.
	scenarioModel = "test/test-model"
	// waitLimit bounds every wait for the bot; a healthy run takes milliseconds.
	waitLimit = 10 * time.Second
	// contactReloadGap is the least time between two loads of the contact
	// list. startBot lowers the bot's limit to it, so that a scenario can see
	// a reload by waiting a little longer.
	contactReloadGap = 50 * time.Millisecond
)

// world is what a scenario starts the bot in.
type world struct {
	// Env holds the bot's settings as environment variables. MODEL defaults
	// to scenarioModel and SIGNAL_DM_USER_IDS to Alice; everything else to
	// the bot's own defaults.
	Env map[string]string
	// Vision says the model list shows the model accepting images.
	Vision bool
	// NoTools says the model list shows the model unable to call tools.
	NoTools bool
	// NoModelList makes the model list fail to load.
	NoModelList bool
	// ModelNotListed leaves the bot's model out of the model list.
	ModelNotListed bool
	// NoBotName leaves the bot's own profile name out of the contact list;
	// otherwise the bot is called "Botty".
	NoBotName bool
	// Contacts are the names in signal-cli's contact list, by id.
	Contacts map[string]string
	// MCP starts two MCP servers and points MCP_CONFIG at them: "stocks"
	// (get_quote, price_chart; /quote needs an argument) and "weather"
	// (get_forecast; /forecast).
	MCP bool
	// BrokenMCP adds a third MCP server, "broken", whose tools fail and echo
	// their arguments: echo_error with an error result, rpc_error with a
	// JSON-RPC error.
	BrokenMCP bool
}

// harness is a running bot and its fakes.
type harness struct {
	t *testing.T
	w world

	writeMu   sync.Mutex // serialises writes to the bot's connection
	mu        sync.Mutex
	conn      net.Conn          // the bot's current signal-cli connection
	conns     int               // connections accepted
	calls     []rpcCall         // every request the bot made to signal-cli
	contacts  map[string]string // the contact list, by id
	failSends bool
	replies   []modelReply   // queued model replies; then "ok"
	reqs      []modelRequest // every chat completions request
	mcpCalls  []string       // "server/tool arguments" for every MCP tool call
	logs      []logLine
	partial   []byte   // a log line still being written
	secrets   []string // message and reply texts, which must never be logged
	sendsRead int      // sends returned by nextSend

	signalSock string // the fake signal-cli's socket
	modelURL   string // the fake OpenRouter's base URL
	mcpConfig  string // the MCP_CONFIG file, if w.MCP

	// Set by startBot.
	handled  chan struct{} // one value per received frame the bot has handled
	settle   func()        // waits until no message or contact reload is being worked on
	shutdown func()        // the bot's shutdown, short of closing the socket
}

// rpcCall is one request the bot made to signal-cli.
type rpcCall struct {
	Method string
	Params map[string]any
}

// sentMsg is one message the bot sent.
type sentMsg struct {
	GroupID, Recipient, Message string
	Mentions, Attachments       []string
}

// modelReply is what the fake OpenRouter answers one request with: Text, a
// call of Tool with Args, or an error Status with Body. A tool call's id is
// "call_N" for the Nth request. Hold, if set, delays the answer until it is
// closed (see hold).
type modelReply struct {
	Text       string
	Tool, Args string
	Status     int
	Body       string
	Hold       chan struct{}
}

// modelRequest is one chat completions request the bot made.
type modelRequest struct {
	Model      string
	System     string        // the system prompt
	Messages   []chatMessage // the rest of the conversation
	Tools      []string      // function names, and server tools by type
	ToolChoice string        // the function the model must call, or ""
	Body       map[string]any
}

// chatMessage is one message of a model request.
type chatMessage struct {
	Role, Content string
	Images        []string // image URLs sent with Content
	ToolCallID    string
	ToolCalls     []string // function names
}

// logLine is one line of the bot's log, with its syslog priority.
type logLine struct {
	Pri  int
	Text string
}

func newHarness(t *testing.T, w world) *harness {
	h := &harness{t: t, w: w, contacts: map[string]string{}, handled: make(chan struct{}, 64)}
	if !w.NoBotName {
		h.contacts[idBot] = "Botty"
	}
	for id, name := range w.Contacts {
		h.contacts[id] = name
	}
	h.startSignal()
	h.startModel()
	if w.MCP || w.BrokenMCP {
		h.startMCP()
	}
	return h
}

// env is the bot's settings: the scenario's, over the harness defaults.
func (h *harness) env() map[string]string {
	e := map[string]string{"MODEL": scenarioModel, "SIGNAL_DM_USER_IDS": idAlice}
	for k, v := range h.w.Env {
		e[k] = v
	}
	return e
}

// --- signal-cli ---------------------------------------------------------------

func (h *harness) startSignal() {
	dir, err := os.MkdirTemp("", "sc") // short: unix socket paths are limited
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { os.RemoveAll(dir) })
	h.signalSock = filepath.Join(dir, "s")
	ln, err := net.Listen("unix", h.signalSock)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			h.mu.Lock()
			h.conn, h.conns = c, h.conns+1
			h.mu.Unlock()
			go h.serveSignal(c)
		}
	}()
}

func (h *harness) serveSignal(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
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
		if json.Unmarshal(line, &req) != nil {
			continue
		}
		h.mu.Lock()
		h.calls = append(h.calls, rpcCall{req.Method, req.Params})
		fail := h.failSends
		h.mu.Unlock()
		var result string
		switch req.Method {
		case "listAccounts":
			result = `[{"number":null,"aci":"` + idBot + `"}]`
		case "listContacts":
			result = h.contactList()
		case "getAttachment":
			result = `{"data":"` + base64.StdEncoding.EncodeToString(scenarioPNG()) + `"}`
		case "getSticker":
			result = `{"data":"` + base64.StdEncoding.EncodeToString([]byte("RIFF\x00\x00\x00\x00WEBPVP8 \x00\x00\x00\x00")) + `"}`
		case "listStickerPacks":
			result = `[{"packId":"0000aaaa","title":"Dogs","stickers":[]},` +
				`{"packId":"00aabbccddeeff00","title":"My Daily Life","stickers":[{"id":1,"emoji":"❤️"},{"id":2,"emoji":"🕵️"}]}]`
		case "listGroups":
			result = `[{"id":"` + idGroup + `","name":"Test group","isMember":true,"members":[{"uuid":"` + idAlice + `"},{"uuid":"` + idBob + `"},{"uuid":"` + idBot + `"}]}]`
		case "send":
			if fail {
				h.write(c, fmt.Sprintf(`{"jsonrpc":"2.0","error":{"code":-1,"message":"Failed to send message"},"id":%s}`, req.ID))
				continue
			}
			result = `{"timestamp":1,"results":[{"type":"SUCCESS"}]}`
		case "sendTyping":
			result = `{"timestamp":1}`
		default:
			continue
		}
		h.write(c, fmt.Sprintf(`{"jsonrpc":"2.0","result":%s,"id":%s}`, result, req.ID))
	}
}

func (h *harness) write(c net.Conn, line string) {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()
	c.Write([]byte(line + "\n"))
}

func (h *harness) contactList() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var cs []map[string]any
	for id, name := range h.contacts {
		cs = append(cs, map[string]any{"number": nil, "uuid": id, "profile": map[string]any{"givenName": name}})
	}
	b, _ := json.Marshal(cs)
	if cs == nil {
		return "[]"
	}
	return string(b)
}

// setContacts replaces signal-cli's contact list (the bot's own name stays).
func (h *harness) setContacts(c map[string]string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	bot, ok := h.contacts[idBot]
	h.contacts = map[string]string{}
	if ok {
		h.contacts[idBot] = bot
	}
	for id, name := range c {
		h.contacts[id] = name
	}
}

// failSending makes signal-cli refuse every send.
func (h *harness) failSending() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.failSends = true
}

// receive delivers a message to the bot, as signal-cli does, and waits until
// the bot has read it. The texts in it are remembered, to check that none is
// logged.
func (h *harness) receive(frame string) {
	h.t.Helper()
	var f struct {
		Envelope struct {
			DataMessage struct {
				Message string
				Quote   struct{ Text string }
			}
		}
	}
	if err := json.Unmarshal([]byte(frame), &f); err != nil {
		h.t.Fatalf("bad frame: %v", err)
	}
	h.keepSecret(f.Envelope.DataMessage.Message)
	h.keepSecret(f.Envelope.DataMessage.Quote.Text)
	h.receiveRaw(`{"jsonrpc":"2.0","method":"receive","params":` + frame + `}`)
	select {
	case <-h.handled:
	case <-time.After(waitLimit):
		h.t.Fatal("the bot did not read the message")
	}
}

// receiveRaw writes one line to the bot's connection.
func (h *harness) receiveRaw(line string) {
	h.t.Helper()
	h.mu.Lock()
	c := h.conn
	h.mu.Unlock()
	if c == nil {
		h.t.Fatal("the bot is not connected")
	}
	h.write(c, line)
}

// keepSecret remembers a text that must never reach the log. A command's
// name may be logged, so only what follows it is kept.
func (h *harness) keepSecret(s string) {
	s = strings.TrimSpace(strings.ReplaceAll(s, "￼", ""))
	if strings.HasPrefix(s, "/") {
		if i := strings.IndexFunc(s, unicode.IsSpace); i >= 0 {
			s = s[i:]
		} else {
			s = ""
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.secrets = append(h.secrets, secretForms(s)...)
}

// secretForms is what of s to look for in the log: s as a log line would
// hold it (newlines escaped), each of its lines, and the start of each line,
// so that a leak cut short or quoted is caught too. Pieces shorter than 8
// runes could be anything, and are left out.
func secretForms(s string) []string {
	var out []string
	add := func(x string) {
		if x = strings.TrimSpace(x); utf8.RuneCountInString(x) >= 8 && !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	add(strings.NewReplacer("\n", `\n`, "\r", `\r`).Replace(strings.TrimSpace(s)))
	for _, line := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' }) {
		add(line)
		if r := []rune(strings.TrimSpace(line)); len(r) > 24 {
			add(string(r[:24]))
		}
	}
	return out
}

// callsTo is the parameters of every request the bot made to signal-cli
// with this method.
func (h *harness) callsTo(method string) []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []map[string]any
	for _, c := range h.calls {
		if c.Method == method {
			out = append(out, c.Params)
		}
	}
	return out
}

// methods is the method of every request the bot made to signal-cli, in order.
func (h *harness) methods() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, c := range h.calls {
		out = append(out, c.Method)
	}
	return out
}

// sends is every message the bot sent.
func (h *harness) sends() []sentMsg {
	var out []sentMsg
	for _, p := range h.callsTo("send") {
		s := sentMsg{}
		s.GroupID, _ = p["groupId"].(string)
		if r, _ := p["recipient"].([]any); len(r) > 0 {
			s.Recipient, _ = r[0].(string)
		}
		s.Message, _ = p["message"].(string)
		s.Mentions, s.Attachments = strs(p["mention"]), strs(p["attachments"])
		out = append(out, s)
	}
	return out
}

func strs(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, x := range l {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

// nextSend waits for the next message the bot sends.
func (h *harness) nextSend() sentMsg {
	h.t.Helper()
	var s []sentMsg
	h.waitFor("a message from the bot", func() bool { s = h.sends(); return len(s) > h.sendsRead })
	h.sendsRead++
	return s[h.sendsRead-1]
}

// noMoreSends waits until the bot is idle, and fails if it sent anything
// nextSend has not returned.
func (h *harness) noMoreSends() {
	h.t.Helper()
	h.settle()
	if s := h.sends(); len(s) > h.sendsRead {
		h.t.Fatalf("unexpected messages from the bot: %+v", s[h.sendsRead:])
	}
}

// waitFor polls until ok, failing after waitLimit.
func (h *harness) waitFor(what string, ok func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for !ok() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// --- OpenRouter -----------------------------------------------------------------

func (h *harness) startModel() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, `{"error":{"message":"no auth"}}`, http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet && h.w.NoModelList {
			http.Error(w, `{"error":{"message":"unavailable"}}`, http.StatusServiceUnavailable)
			return
		}
		if r.Method == http.MethodGet { // the model list
			in, params := `["text"]`, `["reasoning","tools"]`
			if h.w.Vision {
				in = `["text","image"]`
			}
			if h.w.NoTools {
				params = `["reasoning"]`
			}
			model := h.env()["MODEL"]
			if h.w.ModelNotListed {
				model = "other/unlisted-model"
			}
			fmt.Fprintf(w, `{"data":[{"id":"other/model","architecture":{"input_modalities":["text"]},"supported_parameters":[]},`+
				`{"id":%q,"architecture":{"input_modalities":%s},"supported_parameters":%s}]}`, model, in, params)
			return
		}
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.reqs = append(h.reqs, parseModelRequest(body))
		n := len(h.reqs)
		reply := modelReply{Text: "ok"}
		if len(h.replies) > 0 {
			reply, h.replies = h.replies[0], h.replies[1:]
		}
		h.mu.Unlock()
		if reply.Hold != nil {
			select {
			case <-reply.Hold:
			case <-r.Context().Done():
				return
			}
		}
		switch {
		case reply.Status != 0:
			w.WriteHeader(reply.Status)
			io.WriteString(w, reply.Body)
		case reply.Tool != "":
			args, _ := json.Marshal(reply.Args)
			fmt.Fprintf(w, `{"choices":[{"message":{"content":"","tool_calls":[{"id":"call_%d","type":"function","function":{"name":%q,"arguments":%s}}]}}],`+
				`"usage":{"prompt_tokens":10,"completion_tokens":2,"cost":0.001}}`, n, reply.Tool, args)
		default:
			text, _ := json.Marshal(reply.Text)
			fmt.Fprintf(w, `{"choices":[{"message":{"content":%s}}],"usage":{"prompt_tokens":20,"completion_tokens":3,"cost":0.002}}`, text)
		}
	}))
	h.t.Cleanup(srv.Close)
	h.modelURL = srv.URL
}

func parseModelRequest(body []byte) modelRequest {
	var raw map[string]any
	json.Unmarshal(body, &raw)
	var b struct {
		Model    string `json:"model"`
		Messages []struct {
			Role       string          `json:"role"`
			Content    json.RawMessage `json:"content"`
			ToolCallID string          `json:"tool_call_id"`
			ToolCalls  []struct {
				Function struct{ Name string } `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
		Tools []struct {
			Type     string `json:"type"`
			Function struct{ Name string }
		} `json:"tools"`
		ToolChoice struct {
			Function struct{ Name string }
		} `json:"tool_choice"`
	}
	json.Unmarshal(body, &b)
	req := modelRequest{Model: b.Model, ToolChoice: b.ToolChoice.Function.Name, Body: raw}
	for _, t := range b.Tools {
		if t.Type == "function" {
			req.Tools = append(req.Tools, t.Function.Name)
		} else {
			req.Tools = append(req.Tools, t.Type)
		}
	}
	for i, m := range b.Messages {
		cm := chatMessage{Role: m.Role, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			cm.ToolCalls = append(cm.ToolCalls, tc.Function.Name)
		}
		if json.Unmarshal(m.Content, &cm.Content) != nil { // content parts: text, then images
			var parts []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				ImageURL struct {
					URL string `json:"url"`
				} `json:"image_url"`
			}
			json.Unmarshal(m.Content, &parts)
			for _, p := range parts {
				if p.Type == "text" {
					cm.Content += p.Text
				} else {
					cm.Images = append(cm.Images, p.ImageURL.URL)
				}
			}
		}
		if i == 0 && m.Role == "system" {
			req.System = cm.Content
			continue
		}
		req.Messages = append(req.Messages, cm)
	}
	return req
}

// hold is a channel for modelReply.Hold, and the function that closes it.
// It is closed when the test ends too, so that a failed scenario never
// leaves a request hanging.
func (h *harness) hold() (chan struct{}, func()) {
	c := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(c) }) }
	h.t.Cleanup(release)
	return c, release
}

// queue sets the model's next replies, in order.
func (h *harness) queue(replies ...modelReply) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.replies = append(h.replies, replies...)
	for _, r := range replies {
		h.secrets = append(h.secrets, secretForms(r.Text)...)
		h.secrets = append(h.secrets, secretForms(r.Args)...)
	}
}

// requests is every chat completions request so far.
func (h *harness) requests() []modelRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.reqs)
}

// waitRequests waits until the model has been asked n times in all.
func (h *harness) waitRequests(n int) []modelRequest {
	h.t.Helper()
	h.waitFor(strconv.Itoa(n)+" model requests", func() bool { return len(h.requests()) >= n })
	return h.requests()
}

// lastRequest is the latest chat completions request.
func (h *harness) lastRequest() modelRequest {
	h.t.Helper()
	r := h.requests()
	if len(r) == 0 {
		h.t.Fatal("the model was never asked")
	}
	return r[len(r)-1]
}

// contents is each message's role and content, as "role: content".
func (r modelRequest) contents() []string {
	var out []string
	for _, m := range r.Messages {
		out = append(out, m.Role+": "+m.Content)
	}
	return out
}

// last is the request's final message.
func (r modelRequest) last() chatMessage {
	if len(r.Messages) == 0 {
		return chatMessage{}
	}
	return r.Messages[len(r.Messages)-1]
}

// --- MCP ------------------------------------------------------------------------

// fakeMCPTool is a tool a fake MCP server lists, and what calling it returns.
// A tool that fails echoes its arguments: fail is "result" for an error
// result, "rpc" for a JSON-RPC error.
type fakeMCPTool struct {
	name, description, result string
	fail                      string
}

func (h *harness) startMCP() {
	servers := map[string][]fakeMCPTool{
		"stocks": {
			{name: "get_quote", description: "Get the latest price of a stock", result: `{"content":[{"type":"text","text":"AAPL 230.10 USD"}]}`},
			{name: "price_chart", description: "Draw a price chart of a stock", result: `{"content":[{"type":"image","mimeType":"image/png","data":"` +
				base64.StdEncoding.EncodeToString(scenarioPNG()) + `"},{"type":"text","text":"chart of AAPL"}]}`},
		},
		"weather": {
			{name: "get_forecast", description: "Get the weather forecast for a place", result: `{"content":[{"type":"text","text":"sunny, 24C"}]}`},
		},
	}
	h.secrets = append(h.secrets, "AAPL 230.10 USD", "chart of AAPL", "sunny, 24C") // tool results
	if h.w.BrokenMCP {
		servers["broken"] = []fakeMCPTool{
			{name: "echo_error", description: "Always fails, with an error result", fail: "result"},
			{name: "rpc_error", description: "Always fails, with a JSON-RPC error", fail: "rpc"},
		}
	}
	if !h.w.MCP {
		delete(servers, "stocks")
		delete(servers, "weather")
	}
	urls := map[string]string{}
	for name, tools := range servers {
		srv := httptest.NewServer(h.mcpHandler(name, tools))
		h.t.Cleanup(srv.Close)
		urls[name] = srv.URL
	}
	var entries []string
	if h.w.MCP {
		entries = append(entries,
			`"stocks":{"url":"`+urls["stocks"]+`","chat_commands":{"quote":{"tool":"get_quote","require_arg":true}}}`,
			`"weather":{"url":"`+urls["weather"]+`","chat_commands":{"forecast":{"tool":"get_forecast"}}}`)
	}
	if h.w.BrokenMCP {
		entries = append(entries, `"broken":{"url":"`+urls["broken"]+`"}`)
	}
	cfg := `{"mcpServers":{` + strings.Join(entries, ",") + `}}`
	h.mcpConfig = filepath.Join(h.t.TempDir(), "mcp.json")
	if err := os.WriteFile(h.mcpConfig, []byte(cfg), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// mcpHandler is a Streamable HTTP MCP server, answering with JSON.
func (h *harness) mcpHandler(server string, tools []fakeMCPTool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if req.ID == nil { // a notification
			w.WriteHeader(http.StatusAccepted)
			return
		}
		result := `{}`
		switch req.Method {
		case "initialize":
			result = `{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"` + server + `","version":"1"}}`
		case "tools/list":
			var list []map[string]any
			for _, t := range tools {
				list = append(list, map[string]any{"name": t.name, "description": t.description,
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]string{"type": "string"}}}})
			}
			b, _ := json.Marshal(map[string]any{"tools": list})
			result = string(b)
		case "tools/call":
			h.mu.Lock()
			h.mcpCalls = append(h.mcpCalls, server+"/"+req.Params.Name+" "+string(req.Params.Arguments))
			h.mu.Unlock()
			for _, t := range tools {
				if t.name != req.Params.Name {
					continue
				}
				switch echo := "bad input: " + string(req.Params.Arguments); t.fail {
				case "result":
					b, _ := json.Marshal(map[string]any{"content": []any{map[string]string{"type": "text", "text": echo}}, "isError": true})
					result = string(b)
				case "rpc":
					msg, _ := json.Marshal(echo)
					w.Header().Set("Content-Type", "application/json")
					fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"error":{"code":-32602,"message":%s}}`, *req.ID, msg)
					return
				default:
					result = t.result
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":%s}`, *req.ID, result)
	})
}

// toolCalls is every MCP tool call, as "server/tool arguments".
func (h *harness) toolCalls() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.mcpCalls)
}

// --- the log ----------------------------------------------------------------------

// Write takes the bot's log output. Each line is expected to carry its
// journald priority, "<N>".
func (h *harness) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.partial = append(h.partial, p...)
	for {
		i := bytes.IndexByte(h.partial, '\n')
		if i < 0 {
			return len(p), nil
		}
		line := string(h.partial[:i])
		h.partial = h.partial[i+1:]
		l := logLine{Pri: -1, Text: line}
		if len(line) > 3 && line[0] == '<' && line[2] == '>' && line[1] >= '0' && line[1] <= '7' {
			l = logLine{Pri: int(line[1] - '0'), Text: line[3:]}
		}
		h.logs = append(h.logs, l)
	}
}

// logLines is every log line so far.
func (h *harness) logLines() []logLine {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.logs)
}

// logged is the first log line containing every one of parts, if any.
func (h *harness) logged(parts ...string) (logLine, bool) {
	for _, l := range h.logLines() {
		if containsAll(l.Text, parts) {
			return l, true
		}
	}
	return logLine{}, false
}

// waitLog waits for a log line containing every one of parts.
func (h *harness) waitLog(parts ...string) logLine {
	h.t.Helper()
	var l logLine
	h.waitFor(fmt.Sprintf("a log line with %q", parts), func() (ok bool) { l, ok = h.logged(parts...); return ok })
	return l
}

func containsAll(s string, parts []string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

// checkNoSecretsLogged fails if any message or reply text reached the log.
func (h *harness) checkNoSecretsLogged() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, l := range h.logs {
		for _, s := range h.secrets {
			if strings.Contains(l.Text, s) {
				h.t.Errorf("message text %q reached the log: %q", s, l.Text)
			}
		}
	}
}

// --- images -----------------------------------------------------------------------

// scenarioPNG is a 1×1 PNG.
func scenarioPNG() []byte {
	var buf bytes.Buffer
	png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1)))
	return buf.Bytes()
}

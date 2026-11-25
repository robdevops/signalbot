package addon

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Tool is one of an addon's tools.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage // its arguments' JSON Schema; nil is an object with no properties
	// Visibility is who may call it: "bot" for the bot itself (a direct
	// command, a watch), "model" for the model. Nil is the model alone.
	Visibility []string
	// Call runs it. c is nil for a call the model makes, which carries no
	// chat. An error fails the call (DecodeArgs's as invalid params); a
	// Result with IsError is a failure written for people.
	Call func(ctx context.Context, c *Context, args json.RawMessage) (Result, error)
}

// Config is what a Server is made from.
type Config struct {
	Name, Title, Version string // serverInfo; Title is the name people see
	Tools                []Tool
	Now                  func() time.Time // nil is time.Now; tests set it
	// Changed, if set, is called when a read of the outbox lets go of ops
	// the bot has read past, or ones held too long, so the addon can save
	// Outbox(). It isn't called for Queue: Queue's caller knows to save. It
	// can run on several goroutines at once, so saving must allow that.
	Changed func()
}

// Server is an addon's side of the protocol, on stdin and stdout: MCP's
// initialize, tools and resources, the extension's context and outbox,
// cancellation, and sampling the bot's model. Make one with New.
type Server struct {
	cfg   Config
	tools map[string]Tool
	nonce string        // this run's, in each outbox cursor, op ID and request ID
	dirty chan struct{} // the outbox gained ops: tell the bot

	mu           sync.Mutex
	network      string
	networks     []string
	sampling     bool // the bot offered sampling in initialize
	watchReplies bool // the bot takes a watch with Replies
	watchReacts  bool // the bot takes a watch with Reactions
	subscribed   bool
	outbox       []held                 // ops the bot hasn't read past, oldest first
	seq          int64                  // the last op's sequence number
	wire         *wire                  // Run's, while it runs
	asked        int64                  // the last request's sequence number
	pending      map[string]chan answer // the requests to the bot awaiting an answer, by ID
}

// answer is the bot's answer to a request of the addon's: its result, or
// why there is none.
type answer struct {
	result json.RawMessage
	err    error
}

// held is an op waiting in the outbox, with this run's sequence number,
// which cursors count in.
type held struct {
	Held
	seq int64
}

// The outbox's limits: how long an op unread is kept, and the most a page
// holds as sent, well under the 4 MiB line the bot reads.
const (
	keepOps  = time.Hour
	pageSize = 3 << 20
)

// MaxImages is the most images a post may have; the bot also caps them at
// 2 MB in all.
const MaxImages = 2

// maxLine caps one incoming line. The bot's requests are small: a
// command's arguments or one chat message; so are its answers to Sample,
// which the addon block's max_tokens caps.
const maxLine = 1 << 20

// JSON-RPC's error codes, and MCP's for a resource that isn't there.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeNoMethod       = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603
	codeNotFound       = -32002
)

// New makes a Server for an addon with c's tools.
func New(c Config) *Server {
	if c.Now == nil {
		c.Now = time.Now
	}
	s := &Server{cfg: c, tools: map[string]Tool{}, nonce: randomID(4), dirty: make(chan struct{}, 1), pending: map[string]chan answer{}}
	for _, t := range c.Tools {
		s.tools[t.Name] = t
	}
	return s
}

// Network is the chat network the bot said it is on ("signal",
// "telegram"), or "" before initialize or if it said none. On several, it
// is the first: a call's chat's is its Context's Network.
func (s *Server) Network() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.network
}

// Networks are the chat networks the bot said it is on, when on more than
// one; nil otherwise.
func (s *Server) Networks() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.networks)
}

// CanSample reports whether the bot offered sampling in initialize, so
// that Sample may work: it does with the addon block's model permission.
func (s *Server) CanSample() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sampling
}

// WatchReplies reports whether the bot said in initialize that it takes a
// watch with Replies. One that didn't would refuse it.
func (s *Server) WatchReplies() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.watchReplies
}

// WatchReactions reports whether the bot said in initialize that it takes
// a watch with Reactions.
func (s *Server) WatchReactions() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.watchReacts
}

// BotError is the bot's error answer to a request of the addon's, such as
// a sampling request it refused.
type BotError struct {
	Code    int
	Message string
}

func (e *BotError) Error() string { return fmt.Sprintf("the bot refused: %s (%d)", e.Message, e.Code) }

// errStopped is a request's error when Run returns before the bot answers.
var errStopped = errors.New("addon: Run returned before the bot answered")

// Sample asks the bot's model, through MCP's sampling/createMessage, with
// the cost charged to chat, a chat pseudonym the addon may post in now. It
// needs Run running and a bot that offered sampling (CanSample); without
// either it fails at once. When ctx ends first, it tells the bot the
// request is cancelled and returns ctx's error. An error answer is a
// *BotError.
func (s *Server) Sample(ctx context.Context, chat string, req SampleRequest) (SampleResult, error) {
	if err := ctx.Err(); err != nil {
		return SampleResult{}, err
	}
	req.Meta.Chat = chat
	s.mu.Lock()
	w := s.wire
	if w == nil || !s.sampling {
		s.mu.Unlock()
		if w == nil {
			return SampleResult{}, errors.New("addon: Sample needs Run running")
		}
		return SampleResult{}, errors.New("addon: the bot offers no sampling")
	}
	s.asked++
	id := s.nonce + "." + strconv.FormatInt(s.asked, 10)
	got := make(chan answer, 1)
	s.pending[id] = got
	s.mu.Unlock()
	if err := w.send(request{JSONRPC: "2.0", ID: id, Method: "sampling/createMessage", Params: req}); err != nil {
		if s.forget(id) {
			return SampleResult{}, err
		}
	}
	var a answer
	select {
	case a = <-got:
	case <-ctx.Done():
		if s.forget(id) {
			w.send(notification{JSONRPC: "2.0", Method: "notifications/cancelled", Params: struct {
				RequestID string `json:"requestId"`
			}{id}})
			return SampleResult{}, ctx.Err()
		}
		a = <-got // answered as ctx ended
	}
	if a.err != nil {
		return SampleResult{}, a.err
	}
	var r SampleResult
	if err := json.Unmarshal(a.result, &r); err != nil {
		return SampleResult{}, fmt.Errorf("addon: the bot's sampling result: %w", err)
	}
	return r, nil
}

// forget drops a request still awaiting its answer, and reports whether it
// was.
func (s *Server) forget(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.pending[id]
	delete(s.pending, id)
	return ok
}

// settle hands the bot's answer to the request it answers, if one awaits
// it.
func (s *Server) settle(m message) {
	var id string
	if json.Unmarshal(m.ID, &id) != nil {
		return
	}
	s.mu.Lock()
	got := s.pending[id]
	delete(s.pending, id)
	s.mu.Unlock()
	if got == nil {
		return
	}
	switch {
	case m.Error != nil:
		got <- answer{err: &BotError{m.Error.Code, m.Error.Message}}
	case len(m.Result) == 0:
		got <- answer{err: errors.New("addon: the bot's answer has neither result nor error")}
	default:
		got <- answer{result: m.Result}
	}
}

// Queue adds ops to the outbox, giving each its ID, and tells a subscribed
// bot. The bot reads them when it can; save Outbox() afterwards to keep
// them over a restart. It queues none, and says why, if one could never
// reach the bot: a post with more than MaxImages images, or an op too big
// for a page as it is sent.
func (s *Server) Queue(ops ...Op) error {
	_, err := s.QueueIDs(ops...)
	return err
}

// QueueIDs is Queue, returning the ID it gave each op, in order, such as
// for a later post's ReplyTo or a reaction's Post.
func (s *Server) QueueIDs(ops ...Op) (ids []string, err error) {
	for _, o := range ops {
		switch {
		case len(o.Images) > MaxImages:
			return nil, fmt.Errorf("a post has %d images; the most is %d", len(o.Images), MaxImages)
		case sentSize(o) > pageSize:
			return nil, fmt.Errorf("an op is %d bytes as sent; the most is %d", sentSize(o), pageSize)
		}
	}
	if len(ops) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	now := s.cfg.Now()
	for _, o := range ops {
		s.seq++
		o.ID = s.nonce + "." + strconv.FormatInt(s.seq, 10)
		ids = append(ids, o.ID)
		s.outbox = append(s.outbox, held{Held: Held{Op: o, Queued: now}, seq: s.seq})
	}
	s.mu.Unlock()
	s.poke()
	return ids, nil
}

// sentSize is o's size as the bot gets it: JSON inside the JSON string of
// a resource's text, where every quote and backslash is escaped again.
func sentSize(o Op) int {
	data, _ := json.Marshal(o)
	quoted, _ := json.Marshal(string(data))
	return len(quoted)
}

// Outbox is what the bot hasn't read past yet, oldest first, to save.
func (s *Server) Outbox() []Held {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Held, len(s.outbox))
	for i, h := range s.outbox {
		out[i] = h.Held
	}
	return out
}

// Restore puts saved ops back in the outbox. Call it before Run and before
// any Queue, so the ops stay in the order they were queued. They keep their
// IDs, so the bot, which remembers the IDs it applied, never applies one
// twice; a cursor from before the restart is this run's no longer, so the
// bot's next read gets them all.
func (s *Server) Restore(ops []Held) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, h := range ops {
		s.seq++
		s.outbox = append(s.outbox, held{Held: h, seq: s.seq})
	}
}

// Run serves the bot on in and out until in ends (the bot closed stdin:
// it returns nil) or ctx does (it returns ctx's error). Each request is
// answered on its own goroutine, with a context the bot can cancel
// (notifications/cancelled). When Run stops, it cancels the calls still
// running and waits for them, so none acts after it returns. A read from
// in that is still waiting when ctx ends is left to finish on its own.
func (s *Server) Run(ctx context.Context, in io.Reader, out io.Writer) error {
	w := &wire{w: out}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	notified := make(chan struct{})
	s.mu.Lock()
	s.wire = w
	s.mu.Unlock()
	defer func() {
		cancel() // first, so the calls in flight stop
		s.mu.Lock()
		s.wire = nil
		for id, got := range s.pending {
			got <- answer{err: errStopped}
			delete(s.pending, id)
		}
		s.mu.Unlock()
		wg.Wait()
		<-notified
		w.close() // so a Sample still finishing writes nothing more
	}()
	go func() { defer close(notified); s.notifier(ctx, w) }()
	if len(s.Outbox()) > 0 {
		s.poke()
	}
	lines := make(chan []byte)
	failed := make(chan error, 1)
	go func() {
		r := bufio.NewReaderSize(in, 64<<10)
		for {
			line, err := readLine(r)
			if err != nil {
				failed <- err
				return
			}
			if line == nil { // too long, or blank
				continue
			}
			select {
			case lines <- line:
			case <-ctx.Done():
				return
			}
		}
	}()
	var mu sync.Mutex
	inFlight := map[string]context.CancelFunc{} // by request ID
	for {
		var line []byte
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-failed:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case line = <-lines:
		}
		var m message
		if json.Unmarshal(line, &m) != nil {
			w.send(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{codeParse, "parse error"}})
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
		if m.Method == "" { // an answer to a request of the addon's
			if len(m.ID) > 0 {
				s.settle(m)
			}
			continue
		}
		if len(m.ID) == 0 { // a notification
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
			w.send(s.answer(rctx, m))
			mu.Lock()
			delete(inFlight, id)
			mu.Unlock()
			stop()
		}()
	}
}

// DecodeArgs reads a tool's arguments into v; an error fails the call as
// invalid params.
func DecodeArgs(raw json.RawMessage, v any) error { return decode(raw, v) }

// message is one JSON-RPC message read: a request, a notification (no ID)
// or a response to a request of the addon's (no method).
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// request is a request of the addon's to the bot.
type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      string `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

// response is the answer to one request: its result or an error.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// notification is a message to the bot that wants no answer.
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
// the only writer of stdout, which carries nothing but the protocol. Once
// closed, when Run returns, it writes nothing.
type wire struct {
	mu     sync.Mutex
	w      io.Writer
	closed bool
}

func (w *wire) send(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return errStopped
	}
	_, err = w.w.Write(append(data, '\n'))
	return err
}

func (w *wire) close() {
	w.mu.Lock()
	w.closed = true
	w.mu.Unlock()
}

// handler answers one request's params with a result, or an *rpcError.
type handler func(ctx context.Context, params json.RawMessage) (any, error)

// answer runs one request's handler.
func (s *Server) answer(ctx context.Context, m message) response {
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
			resp.Error = &rpcError{codeInternal, "internal error"}
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

// decode reads a request's params into v, or fails as JSON-RPC's invalid
// params.
func decode(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		raw = json.RawMessage("{}")
	}
	if json.Unmarshal(raw, v) != nil {
		return &rpcError{codeInvalidParams, "invalid params"}
	}
	return nil
}

// The MCP methods' results, as the server writes them.
type (
	initializeResult struct {
		ProtocolVersion string       `json:"protocolVersion"`
		Capabilities    capabilities `json:"capabilities"`
		ServerInfo      serverInfo   `json:"serverInfo"`
	}
	capabilities struct {
		Experimental map[string]Capability `json:"experimental"`
		Tools        struct{}              `json:"tools"`
		Resources    struct {
			Subscribe bool `json:"subscribe"`
		} `json:"resources"`
	}
	serverInfo struct {
		Name    string `json:"name"`
		Title   string `json:"title,omitempty"`
		Version string `json:"version"`
	}
	toolInfo struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"inputSchema"`
		Meta        map[string]any  `json:"_meta,omitempty"`
	}
	resourceTemplate struct {
		URITemplate string `json:"uriTemplate"`
		Name        string `json:"name"`
		MimeType    string `json:"mimeType"`
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

// callParams are tools/call's params, with the context the bot puts in
// _meta when it makes the call itself.
type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Meta      struct {
		Context *Context `json:"io.github.robdevops.signalbot/context"`
	} `json:"_meta"`
}

// methods are the MCP methods the server answers.
func (s *Server) methods() map[string]handler {
	return map[string]handler{
		"initialize": func(_ context.Context, raw json.RawMessage) (any, error) {
			var p struct {
				Capabilities struct {
					Experimental map[string]Capability `json:"experimental"`
					Sampling     *struct{}             `json:"sampling"`
				} `json:"capabilities"`
			}
			json.Unmarshal(raw, &p) // optional: a bot that offers no network still gets an answer
			addon := p.Capabilities.Experimental[KeyAddon]
			s.mu.Lock()
			s.network, s.networks = addon.Network, addon.Networks
			s.watchReplies = addon.WatchReplies
			s.watchReacts = addon.WatchReactions
			s.sampling = p.Capabilities.Sampling != nil
			s.mu.Unlock()
			r := initializeResult{ProtocolVersion: Protocol, ServerInfo: serverInfo{Name: s.cfg.Name, Title: s.cfg.Title, Version: s.cfg.Version}}
			r.Capabilities.Experimental = map[string]Capability{KeyAddon: {Version: Version}}
			r.Capabilities.Resources.Subscribe = true
			return r, nil
		},
		"ping": func(context.Context, json.RawMessage) (any, error) { return struct{}{}, nil },
		"tools/list": func(context.Context, json.RawMessage) (any, error) {
			list := make([]toolInfo, 0, len(s.cfg.Tools))
			for _, t := range s.cfg.Tools {
				schema := t.Schema
				if schema == nil {
					schema = json.RawMessage(`{"type":"object"}`)
				}
				info := toolInfo{Name: t.Name, Description: t.Description, InputSchema: schema}
				if t.Visibility != nil {
					info.Meta = map[string]any{KeyVisibility: t.Visibility}
				}
				list = append(list, info)
			}
			return struct {
				Tools []toolInfo `json:"tools"`
			}{list}, nil
		},
		"tools/call": func(ctx context.Context, raw json.RawMessage) (any, error) {
			var p callParams
			if err := decode(raw, &p); err != nil {
				return nil, err
			}
			t, ok := s.tools[p.Name]
			if !ok || t.Call == nil {
				return nil, &rpcError{codeInvalidParams, "unknown tool"}
			}
			r, err := t.Call(ctx, p.Meta.Context, p.Arguments)
			if r.Content == nil {
				r.Content = []Content{} // MCP's content is an array, never null
			}
			return r, err
		},
		"resources/list": func(context.Context, json.RawMessage) (any, error) {
			return struct {
				Resources []struct{} `json:"resources"`
			}{[]struct{}{}}, nil
		},
		"resources/templates/list": func(context.Context, json.RawMessage) (any, error) {
			return struct {
				Templates []resourceTemplate `json:"resourceTemplates"`
			}{[]resourceTemplate{{URITemplate: OutboxURI + "/{cursor}", Name: "outbox", MimeType: "application/json"}}}, nil
		},
		"resources/subscribe":   func(_ context.Context, raw json.RawMessage) (any, error) { return s.subscribe(raw, true) },
		"resources/unsubscribe": func(_ context.Context, raw json.RawMessage) (any, error) { return s.subscribe(raw, false) },
		"resources/read": func(_ context.Context, raw json.RawMessage) (any, error) {
			var p uriParams
			if err := decode(raw, &p); err != nil {
				return nil, err
			}
			cursor, ok := strings.CutPrefix(p.URI, OutboxURI+"/")
			if !ok {
				return nil, &rpcError{codeNotFound, "resource not found"}
			}
			page, err := json.Marshal(s.read(cursor))
			if err != nil {
				return nil, err
			}
			return struct {
				Contents []resourceContent `json:"contents"`
			}{[]resourceContent{{URI: p.URI, MimeType: "application/json", Text: string(page)}}}, nil
		},
	}
}

// subscribe records whether the bot wants to hear of the outbox.
func (s *Server) subscribe(raw json.RawMessage, on bool) (any, error) {
	var p uriParams
	if err := decode(raw, &p); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(p.URI, OutboxURI) {
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

// read is the outbox's page after cursor. A cursor of this run lets go of
// the ops up to it, which the bot has read; any other, such as one from
// before a restart, gets everything still held.
func (s *Server) read(cursor string) Page {
	s.mu.Lock()
	after, known := s.cursor(cursor)
	now := s.cfg.Now()
	n := len(s.outbox)
	s.outbox = slices.DeleteFunc(s.outbox, func(h held) bool {
		return known && h.seq <= after || now.Sub(h.Queued) > keepOps
	})
	changed := len(s.outbox) != n
	last := s.seq
	if known {
		last = after
	}
	page := Page{Ops: []Op{}}
	size := 0
	for _, h := range s.outbox {
		data := sentSize(h.Op)
		if len(page.Ops) > 0 && size+data > pageSize {
			page.More = true
			break
		}
		size += data
		page.Ops = append(page.Ops, h.Op)
		last = h.seq
	}
	page.Cursor = s.nonce + "." + strconv.FormatInt(last, 10)
	s.mu.Unlock()
	if changed && s.cfg.Changed != nil {
		s.cfg.Changed()
	}
	return page
}

// cursor reads a cursor of this run: the sequence number read up to.
// s.mu is held.
func (s *Server) cursor(c string) (int64, bool) {
	nonce, seq, ok := strings.Cut(c, ".")
	if !ok || nonce != s.nonce {
		return 0, false
	}
	n, err := strconv.ParseInt(seq, 10, 64)
	if err != nil || n < 0 || n > s.seq {
		return 0, false
	}
	return n, true
}

// notifier tells a subscribed bot each time the outbox gains ops, on its
// own goroutine, as a write can wait on a bot that is busy.
func (s *Server) notifier(ctx context.Context, w *wire) {
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
			w.send(notification{JSONRPC: "2.0", Method: "notifications/resources/updated", Params: uriParams{OutboxURI}})
		}
	}
}

// poke marks the outbox changed, without waiting.
func (s *Server) poke() {
	select {
	case s.dirty <- struct{}{}:
	default:
	}
}

// randomID is n random bytes in hex.
func randomID(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

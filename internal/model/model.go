// Package model asks the model through a chat completions API in OpenAI's
// format, OpenRouter's by default, and runs the tool loop.
package model

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/robdevops/signalbot/internal/chats"
	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/httpx"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

const (
	// maxRounds caps the requests one reply may take while the model keeps
	// calling tools.
	maxRounds = 5
	// budget bounds one whole reply, tool rounds included.
	budget = 3 * time.Minute
)

var retryDelay = time.Second

// ErrImages and ErrTools mean an API other than OpenRouter's rejected a
// request for carrying images, or tools: capabilities there are assumed,
// so the caller turns that one off and asks again.
var (
	ErrImages = errors.New("images rejected")
	ErrTools  = errors.New("tools rejected")
)

// statusError is an error response, reduced to its status and message.
type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return fmt.Sprintf("status %d: %s", e.code, e.msg) }

// Client talks to the model's API.
type Client struct {
	key, model, reasoning, base string
	openRouter                  bool // the API is OpenRouter's
	http                        *http.Client
}

// New makes the client of cfg.APIURL, keeping one idle connection per reply
// that can run at once. Only OpenRouter's API (in any case, on any port)
// gets OpenRouter's extras, model list and server tools.
func New(cfg *config.Config) *Client {
	host := "openrouter.ai"
	if cfg.Test != nil && cfg.Test.OpenRouter != "" {
		host = cfg.Test.OpenRouter
	}
	u, err := url.Parse(cfg.APIURL)
	return &Client{key: cfg.APIKey, model: cfg.Model, reasoning: cfg.Reasoning, base: cfg.APIURL,
		openRouter: err == nil && strings.EqualFold(u.Hostname(), host), http: httpx.Client(cfg.MaxConcurrency)}
}

// Usable is the tools the API can run: every function tool, and a server
// tool ("provider:name") only on the provider it names, of which only
// OpenRouter ("openrouter") is known. Each one left out is logged.
func (c *Client) Usable(tools []feature.Tool) []feature.Tool {
	var out []feature.Tool
	for _, t := range tools {
		if prefix, _, _ := strings.Cut(t.Server, ":"); t.Server != "" && !(c.openRouter && prefix == "openrouter") {
			logx.Warnf("tool %s left out: it runs only on %s's API", t.Server, prefix)
			continue
		}
		out = append(out, t)
	}
	return out
}

// Request is one question for the model.
type Request struct {
	System     string
	History    []chats.Entry
	Text       string
	Images     []string // data: URLs shown with Text
	Tools      []feature.Tool
	ToolChoice string // a function tool the model must call before it answers
}

// Reply is the model's answer.
type Reply struct {
	Text      string
	Pictures  []feature.Picture
	ToolsUsed []string // the tools the model ran, as the tool loop reports them
}

// message is a chat message as the API takes it.
type message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Images     []string   `json:"-"`
}

// MarshalJSON sends a message with images as content parts: the text, then
// each image.
func (m message) MarshalJSON() ([]byte, error) {
	type plain message // without this method
	if len(m.Images) == 0 {
		return json.Marshal(plain(m))
	}
	parts := []any{map[string]string{"type": "text", "text": m.Content}}
	for _, u := range m.Images {
		parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": u}})
	}
	return json.Marshal(struct {
		plain
		Content []any `json:"content"`
	}{plain(m), parts})
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // JSON, as the model wrote it
	} `json:"function"`
}

// answer is one completion and what it cost.
type answer struct {
	text                  string
	calls                 []toolCall
	input, cached, output int
	cost                  float64
	costed                bool // the API reported the cost
}

// Ask asks the model and returns its reply. When the model calls function
// tools it runs them and asks again, up to maxRounds requests in budget,
// keeping up to MaxPictures pictures from them. The tokens of every request
// are summed and logged, with the cost when the API reports it.
func (c *Client) Ask(ctx context.Context, r Request) (Reply, error) {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	start := time.Now()
	msgs := []message{{Role: "system", Content: r.System}}
	for _, e := range r.History {
		msgs = append(msgs, message{Role: e.Role, Content: e.Content})
	}
	msgs = append(msgs, message{Role: "user", Content: r.Text, Images: r.Images})
	tools, defs := map[string]feature.Tool{}, []any{}
	for _, t := range r.Tools {
		if t.Server != "" {
			defs = append(defs, map[string]string{"type": t.Server})
			continue
		}
		schema := t.Schema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		defs = append(defs, map[string]any{"type": "function", "function": map[string]any{
			"name": t.Name, "description": t.Description, "parameters": schema}})
		tools[t.Name] = t
	}
	choice := ""
	if _, ok := tools[r.ToolChoice]; ok {
		choice = r.ToolChoice
	}

	var reply Reply
	var total answer
	for round := 1; ; round++ {
		a, err := c.request(ctx, msgs, defs, choice)
		if round == 1 {
			err = c.rejected(err, len(r.Images) > 0, len(defs) > 0)
		}
		total.input, total.cached, total.output, total.cost = total.input+a.input, total.cached+a.cached, total.output+a.output, total.cost+a.cost
		total.costed = total.costed || a.costed
		if err != nil {
			return reply, err
		}
		if len(a.calls) == 0 || round == maxRounds {
			if len(a.calls) > 0 {
				logx.Warnf("model still calling tools after %d requests; stopping", round)
				a.text = cmp.Or(a.text, "Sorry, I couldn't finish that.")
			}
			if reply.Text = a.text; reply.Text == "" && len(reply.Pictures) == 0 {
				return reply, errors.New("empty response") // not the body: it can hold the conversation
			}
			cached := 100 * min(total.cached, total.input) / max(total.input, 1)
			cost := ""
			if total.costed {
				cost = fmt.Sprintf(", $%.3f", total.cost)
			}
			logx.Infof("reply %.1fs, tokens in/out: %d/%d (%d%% cached)%s",
				time.Since(start).Seconds(), total.input, total.output, cached, cost)
			return reply, nil
		}
		msgs = append(msgs, message{Role: "assistant", Content: a.text, ToolCalls: a.calls})
		for _, tc := range a.calls {
			name := tc.Function.Name
			out, pics, err := run(ctx, tools[name], tc.Function.Arguments)
			if err != nil {
				out = cmp.Or(out, "error: "+err.Error())
			} else {
				reply.ToolsUsed = append(reply.ToolsUsed, name)
			}
			if name == choice {
				choice = "" // its result is in
			}
			reply.Pictures = append(reply.Pictures, pics[:min(len(pics), feature.MaxPictures-len(reply.Pictures))]...)
			msgs = append(msgs, message{Role: "tool", ToolCallID: tc.ID, Content: out})
		}
	}
}

// rejected is err as ErrImages if the first request carried images and an
// API other than OpenRouter's refused it with a 4xx about the request
// itself (not the key, the credit or the rate), else as ErrTools if it
// carried tools; any other error is left as it is.
func (c *Client) rejected(err error, images, tools bool) error {
	var se *statusError
	if c.openRouter || !errors.As(err, &se) || se.code < 400 || se.code > 499 ||
		slices.Contains([]int{401, 402, 403, 429}, se.code) {
		return err
	}
	switch {
	case images:
		return fmt.Errorf("%w: %v", ErrImages, err)
	case tools:
		return fmt.Errorf("%w: %v", ErrTools, err)
	}
	return err
}

// run runs one function tool and logs how it went, never its arguments or
// result.
func run(ctx context.Context, t feature.Tool, args string) (out string, pics []feature.Picture, err error) {
	if t.Run == nil {
		logx.Warnf("model called a tool it wasn't offered")
		return "error: there is no such tool", nil, errors.New("unknown tool")
	}
	start := time.Now()
	defer func() {
		if recover() != nil {
			out, pics, err = "", nil, errors.New("the tool failed")
		}
		if err != nil {
			logx.Warnf("tool %s failed after %.1fs: %v", t.Name, time.Since(start).Seconds(), err)
			logx.Debugf("tool %s arguments were %d bytes", t.Name, len(args))
		} else {
			logx.Infof("tool %s %.1fs", t.Name, time.Since(start).Seconds())
		}
	}()
	return t.Run(ctx, args)
}

// request makes one chat completions request. On OpenRouter it routes only
// to providers that don't collect the data, and asks for the cost to be
// reported.
func (c *Client) request(ctx context.Context, msgs []message, tools []any, choice string) (answer, error) {
	body := map[string]any{"model": c.model, "messages": msgs}
	effort := c.reasoning != "" && c.reasoning != "none"
	switch {
	case c.openRouter:
		body["provider"] = map[string]string{"data_collection": "deny"}
		body["usage"] = map[string]bool{"include": true}
		if effort {
			body["reasoning"] = map[string]string{"effort": c.reasoning}
		}
	case effort:
		body["reasoning_effort"] = c.reasoning // the standard parameter
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	if choice != "" {
		body["tool_choice"] = map[string]any{"type": "function", "function": map[string]string{"name": choice}}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return answer{}, err
	}
	data, err := c.post(ctx, payload)
	if err != nil {
		return answer{}, err
	}
	var r struct {
		Choices []struct {
			Message struct {
				Content   string     `json:"content"`
				ToolCalls []toolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        int      `json:"prompt_tokens"`
			CompletionTokens    int      `json:"completion_tokens"`
			Cost                *float64 `json:"cost"` // OpenRouter's
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(data, &r) != nil {
		return answer{}, errors.New("malformed response") // not the error: it can quote the body
	}
	a := answer{input: r.Usage.PromptTokens, cached: r.Usage.PromptTokensDetails.CachedTokens,
		output: r.Usage.CompletionTokens, costed: r.Usage.Cost != nil}
	if a.costed {
		a.cost = *r.Usage.Cost
	}
	if len(r.Choices) > 0 {
		a.text = strings.TrimSpace(r.Choices[0].Message.Content)
		a.calls = r.Choices[0].Message.ToolCalls
	}
	return a, nil
}

// post sends payload and returns the body of a 200 response. A network
// error, 429 or 5xx gets one retry.
func (c *Client) post(ctx context.Context, payload []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	for attempt := 1; ; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/chat/completions", bytes.NewReader(payload))
		c.authorize(req)
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.http.Do(req)
		if err == nil {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return data, nil
			}
			err = &statusError{resp.StatusCode, errorText(data)}
			if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
				return nil, err
			}
		}
		if attempt == 2 || ctx.Err() != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
		case <-time.After(retryDelay):
		}
	}
}

func (c *Client) authorize(req *http.Request) {
	if c.key != "" {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
}

// errorText is the message in an API error body, for the log. Only the
// message is kept: other fields can quote the request (moderation errors
// include the flagged input).
func errorText(data []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &e) != nil {
		return "(unreadable error body)"
	}
	return cmp.Or(txt.Clamp(e.Error.Message, 200), "(no message)")
}

// Capabilities reads from OpenRouter's model list whether the model takes
// images and can call tools. If the list can't tell, images are off
// (sending them would fail every such message) and tools stay on. Any
// other API has no list, so both are assumed until a request carrying them
// is rejected (ErrImages, ErrTools). Either way it says so.
func (c *Client) Capabilities(ctx context.Context) (images, tools bool) {
	if !c.openRouter {
		logx.Noticef("only OpenRouter's model list says what a model supports, so images and tools are on until the API rejects them")
		return true, true
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/models", nil)
	c.authorize(req)
	resp, err := c.http.Do(req)
	if err != nil {
		logx.Warnf("could not check what %s supports, so images are off: %v", c.model, err)
		return false, true
	}
	defer resp.Body.Close()
	var r struct {
		Data []struct {
			ID           string `json:"id"`
			Architecture struct {
				InputModalities []string `json:"input_modalities"`
			} `json:"architecture"`
			SupportedParameters []string `json:"supported_parameters"`
		} `json:"data"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&r) != nil {
		logx.Warnf("could not check what %s supports, so images are off: status %d", c.model, resp.StatusCode)
		return false, true
	}
	for _, m := range r.Data {
		if m.ID == c.model {
			return slices.Contains(m.Architecture.InputModalities, "image"), slices.Contains(m.SupportedParameters, "tools")
		}
	}
	logx.Warnf("model %s is not in OpenRouter's model list, so images are off", c.model)
	return false, true
}

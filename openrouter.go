package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type ChatMsg struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // an assistant turn that calls tools
	ToolCallID string     `json:"tool_call_id,omitempty"` // a tool's answer to that call

	// Expires is when this message must be forgotten (disappearing messages);
	// zero means it is kept until pushed out. Never sent to the model.
	Expires time.Time `json:"-"`
}

// ToolCall is the model asking to run a tool.
type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"` // JSON, as the model wrote it
	} `json:"function"`
}

// Completion is a model reply plus what it cost.
type Completion struct {
	Text         string
	InputTokens  int // all input tokens, cached ones included
	CachedTokens int // the part of InputTokens served from the prompt cache
	OutputTokens int
	Cost         float64 // USD, as OpenRouter reports it; 0 if it did not
	ToolCalls    []ToolCall
}

// Completer is a model API the bot can ask for a reply.
type Completer interface {
	Complete(ctx context.Context, system string, msgs []ChatMsg) (Completion, error)
}

// OpenRouter talks to OpenRouter's chat completions API.
type OpenRouter struct {
	cfg   *Config
	url   string
	http  *http.Client
	tools *MCP // nil without MCP_CONFIG
}

// newHTTPClient is Go's default transport (proxy settings, timeouts, HTTP/2),
// but refusing anything older than TLS 1.3.
func newHTTPClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13}
	return &http.Client{Transport: t}
}

// NewOpenRouter makes the client; tools, which may be nil, are the MCP tools
// the model may call.
func NewOpenRouter(cfg *Config, tools *MCP) *OpenRouter {
	return &OpenRouter{cfg: cfg, url: "https://openrouter.ai/api/v1/chat/completions", http: newHTTPClient(), tools: tools}
}

const (
	// maxToolRounds caps the model requests one reply may take when the
	// model keeps calling tools.
	maxToolRounds = 5
	// replyBudget bounds one whole reply, tool rounds included.
	replyBudget = 3 * time.Minute
)

// Complete sends the conversation and returns the reply text. When the model
// calls MCP tools, it runs them and asks again, up to maxToolRounds requests.
// Usage and cost are summed over all of them.
func (o *OpenRouter) Complete(ctx context.Context, system string, msgs []ChatMsg) (Completion, error) {
	ctx, cancel := context.WithTimeout(ctx, replyBudget)
	defer cancel()
	msgs = append([]ChatMsg{{Role: "system", Content: system}}, msgs...)
	var total Completion
	for round := 1; ; round++ {
		c, err := o.request(ctx, msgs)
		total.InputTokens += c.InputTokens
		total.CachedTokens += c.CachedTokens
		total.OutputTokens += c.OutputTokens
		total.Cost += c.Cost
		if err != nil {
			return total, err
		}
		if len(c.ToolCalls) == 0 || o.tools == nil {
			if total.Text = c.Text; total.Text == "" {
				return total, fmt.Errorf("empty response")
			}
			return total, nil
		}
		if round == maxToolRounds {
			warnf("model still calling tools after %d requests; stopping", round)
			total.Text = cmp.Or(c.Text, "Sorry, I couldn't finish that.")
			return total, nil
		}
		msgs = append(msgs, ChatMsg{Role: "assistant", Content: c.Text, ToolCalls: c.ToolCalls})
		for _, tc := range c.ToolCalls {
			start := time.Now()
			out, t, err := o.tools.Call(ctx, tc.Function.Name, tc.Function.Arguments)
			tool := cmp.Or(t.Name, "unknown") + " (" + cmp.Or(t.Server, "no server") + ")"
			if err != nil {
				warnf("tool %s failed after %.1fs: %v", tool, time.Since(start).Seconds(), err)
			} else {
				infof("tool %s %.1fs", tool, time.Since(start).Seconds())
			}
			msgs = append(msgs, ChatMsg{Role: "tool", ToolCallID: tc.ID, Content: out})
		}
	}
}

// request makes one chat completions request. It asks OpenRouter to route
// only to providers that do not collect the data, and for the real cost to
// be reported back.
func (o *OpenRouter) request(ctx context.Context, msgs []ChatMsg) (Completion, error) {
	body := map[string]any{
		"model":    o.cfg.Model,
		"messages": msgs,
		"provider": map[string]string{"data_collection": "deny"},
		"usage":    map[string]bool{"include": true},
	}
	if r := o.cfg.Reasoning; r != "" && r != "none" {
		body["reasoning"] = map[string]string{"effort": r}
	}
	var tools []any
	if o.cfg.Search {
		// A server tool the model calls only when it decides to search, unlike
		// the deprecated "web" plugin, which searched on every request.
		tools = append(tools, map[string]string{"type": "openrouter:web_search"})
	}
	if o.tools != nil {
		for _, t := range o.tools.tools {
			schema := t.Schema
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object"}`)
			}
			tools = append(tools, map[string]any{"type": "function", "function": map[string]any{
				"name": t.Model, "description": t.Description, "parameters": schema,
			}})
		}
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Completion{}, err
	}
	data, err := o.post(ctx, payload)
	if err != nil {
		return Completion{}, err
	}
	return parseChatCompletion(data)
}

// post sends payload and returns the body of a 200 response. A network error,
// 429 or 5xx gets one retry.
func (o *OpenRouter) post(ctx context.Context, payload []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	const attempts = 2
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, o.url, bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+o.cfg.APIKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := o.http.Do(req)
		if err != nil {
			lastErr = err
		} else {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return data, nil
			}
			lastErr = fmt.Errorf("OpenRouter status %d: %s", resp.StatusCode, apiErrorText(data))
			if resp.StatusCode != 429 && resp.StatusCode < 500 {
				return nil, lastErr
			}
		}
		if attempt == attempts || ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	return nil, lastErr
}

// apiErrorText is the message in an API error body, for the log. Only the
// message is kept: other fields can quote the request (OpenRouter's
// moderation errors include the flagged input).
func apiErrorText(data []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &e) != nil {
		return "(unreadable error body)"
	}
	return cmp.Or(clamp(e.Error.Message, 200), "(no message)")
}

func parseChatCompletion(data []byte) (Completion, error) {
	var r struct {
		Choices []struct {
			Message struct {
				Content   string     `json:"content"`
				ToolCalls []ToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        int     `json:"prompt_tokens"`
			CompletionTokens    int     `json:"completion_tokens"`
			Cost                float64 `json:"cost"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return Completion{}, fmt.Errorf("malformed response") // not err: it can quote the body
	}
	c := Completion{
		InputTokens:  r.Usage.PromptTokens,
		CachedTokens: r.Usage.PromptTokensDetails.CachedTokens,
		OutputTokens: r.Usage.CompletionTokens,
		Cost:         r.Usage.Cost,
	}
	if len(r.Choices) > 0 {
		c.Text = strings.TrimSpace(r.Choices[0].Message.Content)
		c.ToolCalls = r.Choices[0].Message.ToolCalls
	}
	if c.Text == "" && len(c.ToolCalls) == 0 {
		return c, fmt.Errorf("empty response") // not the body: it can hold the conversation
	}
	return c, nil
}

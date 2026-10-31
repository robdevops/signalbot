package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"
)

type ChatMsg struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`   // an assistant turn that calls tools
	ToolCallID string     `json:"tool_call_id,omitempty"` // a tool's answer to that call
	Images     []string   `json:"-"`                      // data: URLs shown to the model with Content

	// Expires is when this message must be forgotten (disappearing messages);
	// zero means it is kept until pushed out. Never sent to the model.
	Expires time.Time `json:"-"`
}

// MarshalJSON sends a message with images as content parts: the text, then
// each image.
func (m ChatMsg) MarshalJSON() ([]byte, error) {
	type plain ChatMsg // without this method
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

// Capabilities asks OpenRouter's model list whether the model accepts images
// and tool calls. If it cannot tell, images are off (sending them would fail
// every such message) and tools stay on, and the failure is logged.
func (o *OpenRouter) Capabilities(ctx context.Context) (images, tools bool) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, o.modelsURL, nil)
	req.Header.Set("Authorization", "Bearer "+o.cfg.APIKey)
	resp, err := o.http.Do(req)
	if err != nil {
		warnf("could not check what %s supports, so images are off: %v", o.cfg.Model, err)
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
		warnf("could not check what %s supports, so images are off: status %d", o.cfg.Model, resp.StatusCode)
		return false, true
	}
	for _, m := range r.Data {
		if m.ID == o.cfg.Model {
			return slices.Contains(m.Architecture.InputModalities, "image"), slices.Contains(m.SupportedParameters, "tools")
		}
	}
	warnf("model %s is not in OpenRouter's model list, so images are off", o.cfg.Model)
	return false, true
}

// chartKey marks a request context in which the chart tool is offered.
type chartKey struct{}

// withChart offers the chart tool to the model for requests made with ctx.
func withChart(ctx context.Context) context.Context { return context.WithValue(ctx, chartKey{}, true) }

func chartWanted(ctx context.Context) bool { v, _ := ctx.Value(chartKey{}).(bool); return v }

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
	Images       [][]byte // charts and tool images to send with the reply
	Captions     []string // each image's caption: a chart's title, or ""
	Charted      bool     // a built-in chart tool drew one
}

// Completer is a model API the bot can ask for a reply.
type Completer interface {
	Complete(ctx context.Context, system string, msgs []ChatMsg) (Completion, error)
}

// OpenRouter talks to OpenRouter's chat completions API.
type OpenRouter struct {
	cfg       *Config
	url       string
	modelsURL string
	http      *http.Client
	tools     *MCP // nil without MCP_CONFIG
	noTools   bool // the model can't call tools: offer none
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
	return &OpenRouter{cfg: cfg, url: "https://openrouter.ai/api/v1/chat/completions",
		modelsURL: "https://openrouter.ai/api/v1/models", http: newHTTPClient(), tools: tools}
}

const (
	// maxToolRounds caps the model requests one reply may take when the
	// model keeps calling tools.
	maxToolRounds = 5
	// replyBudget bounds one whole reply, tool rounds included.
	replyBudget = 3 * time.Minute
)

// Complete sends the conversation and returns the reply text. When the model
// calls tools (the chart tool or MCP tools), it runs them and asks again, up
// to maxToolRounds requests. Usage and cost are summed over all of them, and
// images the tools make are returned for the reply.
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
		if len(c.ToolCalls) == 0 {
			if total.Text = c.Text; total.Text == "" && len(total.Images) == 0 {
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
			out, images, caption, tool, err := o.runTool(ctx, tc)
			if err != nil {
				warnf("tool %s failed after %.1fs: %v", tool, time.Since(start).Seconds(), err)
				debugf("tool %s arguments were %d bytes", tool, len(tc.Function.Arguments))
			} else {
				infof("tool %s %.1fs", tool, time.Since(start).Seconds())
				total.Charted = total.Charted || tool == chartTool || tool == donutTool
			}
			for _, img := range images {
				if len(total.Images) < maxReplyImages {
					total.Images = append(total.Images, img)
					total.Captions = append(total.Captions, caption)
				}
			}
			msgs = append(msgs, ChatMsg{Role: "tool", ToolCallID: tc.ID, Content: out})
		}
	}
}

// runTool runs one tool call: a built-in chart tool, or an MCP tool. caption
// is a chart's title and subtitle; tool names it for the log.
func (o *OpenRouter) runTool(ctx context.Context, tc ToolCall) (out string, images [][]byte, caption, tool string, err error) {
	if tc.Function.Name == chartTool {
		s, err := parseChart(tc.Function.Arguments)
		if err != nil {
			return "error: " + err.Error(), nil, "", chartTool, err
		}
		img, err := renderChart(s)
		if err != nil {
			return "error: the chart could not be drawn", nil, "", chartTool, err
		}
		return s.summary(), [][]byte{img}, chartCaption(s.Title, s.Subtitle), chartTool, nil
	}
	if tc.Function.Name == donutTool {
		s, err := parseDonut(tc.Function.Arguments)
		if err != nil {
			return "error: " + err.Error(), nil, "", donutTool, err
		}
		img, err := renderDonut(s)
		if err != nil {
			return "error: the chart could not be drawn", nil, "", donutTool, err
		}
		return s.summary(), [][]byte{img}, chartCaption(s.Title, s.Subtitle), donutTool, nil
	}
	if o.tools == nil {
		return "error: there is no tool named " + tc.Function.Name, nil, "", "unknown", errors.New("unknown tool")
	}
	out, images, t, err := o.tools.Call(ctx, tc.Function.Name, tc.Function.Arguments)
	return out, images, "", cmp.Or(t.Name, "unknown") + " (" + cmp.Or(t.Server, "no server") + ")", err
}

// toolList is the tools offered to the model: web search, the chart tool
// when ctx asks for it, and the MCP tools. None if the model can't call tools.
func (o *OpenRouter) toolList(ctx context.Context) []any {
	if o.noTools {
		return nil
	}
	var tools []any
	if o.cfg.Search {
		// A server tool the model calls only when it decides to search, unlike
		// the deprecated "web" plugin, which searched on every request.
		tools = append(tools, map[string]string{"type": "openrouter:web_search"})
	}
	if chartWanted(ctx) {
		tools = append(tools,
			map[string]any{"type": "function", "function": map[string]any{
				"name": chartTool, "description": chartToolDescription, "parameters": chartToolSchema,
			}},
			map[string]any{"type": "function", "function": map[string]any{
				"name": donutTool, "description": donutToolDescription, "parameters": donutToolSchema,
			}})
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
	return tools
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
	if tools := o.toolList(ctx); len(tools) > 0 {
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

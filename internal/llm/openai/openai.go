// Package openai is a model provider speaking the OpenAI chat completions
// API, as OpenRouter, OpenAI, Ollama, vLLM and others do. Pointed at
// OpenRouter (the default), it also asks that only providers that don't
// collect data are used, reports the real cost, and offers web search.
package openai

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"github.com/robdevops/signalbot/internal/feature"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/robdevops/signalbot/internal/kit/httpx"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
	"github.com/robdevops/signalbot/internal/llm"
)

// OpenRouterURL is the default API base URL.
const OpenRouterURL = "https://openrouter.ai/api/v1"

// Options configure a Client.
type Options struct {
	URL       string // API base URL, e.g. OpenRouterURL
	Key       string // API key; may be "" for a local server
	Model     string
	Reasoning string // effort, or "" / "none" for the provider's default
}

// Client is one provider and model.
type Client struct {
	opt        Options
	openRouter bool
	http       *http.Client
}

// New makes a client for opt.
func New(opt Options) *Client {
	opt.URL = strings.TrimRight(cmp.Or(opt.URL, OpenRouterURL), "/")
	return &Client{opt: opt, openRouter: strings.HasPrefix(opt.URL, "https://openrouter.ai/"), http: httpx.NewClient()}
}

// OpenRouter reports whether the client talks to OpenRouter, whose extras
// (server tools such as web search) it then uses.
func (c *Client) OpenRouter() bool { return c.openRouter }

// name is the provider as error messages call it.
func (c *Client) name() string {
	if c.openRouter {
		return "OpenRouter"
	}
	return "model API"
}

// Chat makes one chat completions request. On OpenRouter it asks to route
// only to providers that do not collect the data, and for the real cost to
// be reported back.
func (c *Client) Chat(ctx context.Context, t llm.Turn) (llm.Completion, error) {
	msgs := make([]message, len(t.Messages))
	for i, m := range t.Messages {
		msgs[i] = message(m)
	}
	body := map[string]any{"model": c.opt.Model, "messages": msgs}
	if c.openRouter {
		body["provider"] = map[string]string{"data_collection": "deny"}
		body["usage"] = map[string]bool{"include": true}
	}
	if r := c.opt.Reasoning; r != "" && r != "none" {
		body["reasoning"] = map[string]string{"effort": r}
	}
	if len(t.Tools) > 0 {
		body["tools"] = t.Tools
		if t.Force != "" {
			body["tool_choice"] = map[string]any{"type": "function", "function": map[string]string{"name": t.Force}}
		}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return llm.Completion{}, err
	}
	data, err := c.post(ctx, payload)
	if err != nil {
		return llm.Completion{}, err
	}
	return parseChatCompletion(data)
}

// post sends payload and returns the body of a 200 response. A network error,
// 429 or 5xx gets one retry.
func (c *Client) post(ctx context.Context, payload []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	const attempts = 2
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.opt.URL+"/chat/completions", bytes.NewReader(payload))
		if c.opt.Key != "" {
			req.Header.Set("Authorization", "Bearer "+c.opt.Key)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.http.Do(req)
		if err != nil {
			lastErr = err
		} else {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return data, nil
			}
			lastErr = fmt.Errorf("%s status %d: %s", c.name(), resp.StatusCode, apiErrorText(data))
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
	return cmp.Or(txt.Clamp(e.Error.Message, 200), "(no message)")
}

func parseChatCompletion(data []byte) (llm.Completion, error) {
	var r struct {
		Choices []struct {
			Message struct {
				Content   string         `json:"content"`
				ToolCalls []llm.ToolCall `json:"tool_calls"`
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
		return llm.Completion{}, fmt.Errorf("malformed response") // not err: it can quote the body
	}
	c := llm.Completion{
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

// message is an llm.Message in the API's JSON.
type message llm.Message

// MarshalJSON sends a message with images as content parts: the text, then
// each image.
func (m message) MarshalJSON() ([]byte, error) {
	plain := struct {
		Role       string         `json:"role"`
		Content    any            `json:"content"`
		ToolCalls  []llm.ToolCall `json:"tool_calls,omitempty"`
		ToolCallID string         `json:"tool_call_id,omitempty"`
	}{m.Role, m.Content, m.ToolCalls, m.ToolCallID}
	if len(m.Images) > 0 {
		parts := []any{map[string]string{"type": "text", "text": m.Content}}
		for _, u := range m.Images {
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": u}})
		}
		plain.Content = parts
	}
	return json.Marshal(plain)
}

// Capabilities asks the provider's model list whether the model accepts
// images and tool calls. If it cannot tell, images are off (sending them
// would fail every such message) and tools stay on, and the failure is
// logged.
func (c *Client) Capabilities(ctx context.Context) (images, tools bool) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.opt.URL+"/models", nil)
	if c.opt.Key != "" {
		req.Header.Set("Authorization", "Bearer "+c.opt.Key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		logx.Warnf("could not check what %s supports, so images are off: %v", c.opt.Model, err)
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
		logx.Warnf("could not check what %s supports, so images are off: status %d", c.opt.Model, resp.StatusCode)
		return false, true
	}
	for _, m := range r.Data {
		if m.ID == c.opt.Model {
			return slices.Contains(m.Architecture.InputModalities, "image"), slices.Contains(m.SupportedParameters, "tools")
		}
	}
	logx.Warnf("model %s is not in %s's model list, so images are off", c.opt.Model, c.name())
	return false, true
}

// SearchTool is OpenRouter's web search server tool, which the model calls
// only when it decides to (unlike the deprecated "web" plugin, which
// searched on every request).
func SearchTool() feature.ToolGroup {
	return feature.ToolGroup{Prompt: "web search", Intro: "web search", Tools: []feature.Tool{{
		Name: "web_search", Label: "web search", Def: map[string]string{"type": "openrouter:web_search"},
	}}}
}

// ServerTools are the tools the provider runs itself: web search on
// OpenRouter when search is on, else none.
func (c *Client) ServerTools(search bool) []feature.ToolGroup {
	if !search || !c.openRouter {
		return nil
	}
	return []feature.ToolGroup{SearchTool()}
}

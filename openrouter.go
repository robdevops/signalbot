package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// OpenRouter talks to OpenRouter's chat completions API.
type OpenRouter struct {
	cfg  *Config
	url  string
	http *http.Client
}

func NewOpenRouter(cfg *Config) *OpenRouter {
	return &OpenRouter{cfg: cfg, url: "https://openrouter.ai/api/v1/chat/completions", http: newHTTPClient()}
}

// Complete sends the conversation and returns the reply text. Requests ask
// OpenRouter to route only to providers that do not collect the data, and
// for the real cost to be reported back.
func (o *OpenRouter) Complete(ctx context.Context, system string, msgs []ChatMsg) (Completion, error) {
	body := map[string]any{
		"model":    o.cfg.Model,
		"messages": append([]ChatMsg{{Role: "system", Content: system}}, msgs...),
		"provider": map[string]string{"data_collection": "deny"},
		"usage":    map[string]bool{"include": true},
	}
	if r := o.cfg.Reasoning; r != "" && r != "none" {
		body["reasoning"] = map[string]string{"effort": r}
	}
	if o.cfg.Search {
		// A server tool the model calls only when it decides to search, unlike
		// the deprecated "web" plugin, which searched on every request.
		body["tools"] = []map[string]string{{"type": "openrouter:web_search"}}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Completion{}, err
	}
	data, err := postJSON(ctx, o.http, "OpenRouter", o.url, o.cfg.APIKey, payload)
	if err != nil {
		return Completion{}, err
	}
	return parseChatCompletion(data)
}

func parseChatCompletion(data []byte) (Completion, error) {
	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens        int      `json:"prompt_tokens"`
			CompletionTokens    int      `json:"completion_tokens"`
			Cost                *float64 `json:"cost"`
			PromptTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return Completion{}, err
	}
	c := Completion{
		InputTokens:  r.Usage.PromptTokens,
		CachedTokens: r.Usage.PromptTokensDetails.CachedTokens,
		OutputTokens: r.Usage.CompletionTokens,
		Billed:       r.Usage.Cost,
	}
	if len(r.Choices) > 0 {
		c.Text = strings.TrimSpace(r.Choices[0].Message.Content)
	}
	if c.Text == "" {
		return c, fmt.Errorf("empty response: %s", clamp(string(data), 200))
	}
	return c, nil
}

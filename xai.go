package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

type ChatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Completion is a model reply plus what it cost.
type Completion struct {
	Text         string
	InputTokens  int // all input tokens, cached ones included
	CachedTokens int // the part of InputTokens served from xAI's prompt cache
	OutputTokens int
	SearchCalls  int
}

// searchCallUSD is xAI's price for one server-side web search call
// ($5 per 1,000), billed on top of tokens.
const searchCallUSD = 5.0 / 1000

// Cost estimates the request's price in USD from the configured per-million
// token rates, billing cached input at its own lower rate. It is an
// estimate: long-prompt pricing tiers are not modelled.
func (c Completion) Cost(cfg *Config) float64 {
	cached := min(c.CachedTokens, c.InputTokens)
	return float64(c.InputTokens-cached)/1e6*cfg.PriceIn +
		float64(cached)/1e6*cfg.PriceCached +
		float64(c.OutputTokens)/1e6*cfg.PriceOut +
		float64(c.SearchCalls)*searchCallUSD
}

type XAI struct {
	cfg  *Config
	url  string
	http *http.Client
}

func NewXAI(cfg *Config) *XAI {
	return &XAI{cfg: cfg, url: "https://api.x.ai/v1/responses", http: http.DefaultClient}
}

// Complete sends the conversation to the x.ai Responses API and returns the reply text.
func (x *XAI) Complete(ctx context.Context, system string, msgs []ChatMsg) (Completion, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	body := map[string]any{
		"model": x.cfg.Model,
		"input": append([]ChatMsg{{Role: "system", Content: system}}, msgs...),
		"store": false,
	}
	if r := x.cfg.Reasoning; r != "" && r != "none" {
		body["reasoning"] = map[string]string{"effort": r}
	}
	if x.cfg.Search {
		body["tools"] = []map[string]string{{"type": "web_search"}}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return Completion{}, err
	}

	const attempts = 2
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, x.url, bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+x.cfg.APIKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := x.http.Do(req)
		if err != nil {
			lastErr = err
		} else {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return parseOutput(data)
			}
			lastErr = fmt.Errorf("x.ai status %d: %s", resp.StatusCode, clamp(string(data), 200))
			if resp.StatusCode != 429 && resp.StatusCode < 500 {
				return Completion{}, lastErr
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
	return Completion{}, lastErr
}

func parseOutput(data []byte) (Completion, error) {
	var r struct {
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			InputTokensDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return Completion{}, err
	}
	var c Completion
	var sb strings.Builder
	for _, o := range r.Output {
		if strings.HasSuffix(o.Type, "search_call") {
			c.SearchCalls++
		}
		if o.Type != "message" {
			continue
		}
		for _, part := range o.Content {
			if part.Type == "output_text" || part.Type == "text" {
				sb.WriteString(part.Text)
			}
		}
	}
	c.Text = strings.TrimSpace(sb.String())
	c.InputTokens = r.Usage.InputTokens
	c.CachedTokens = r.Usage.InputTokensDetails.CachedTokens
	c.OutputTokens = r.Usage.OutputTokens
	if c.Text == "" {
		return c, fmt.Errorf("empty response: %s", clamp(string(data), 200))
	}
	return c, nil
}

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
	Role    string `json:"role"`
	Content string `json:"content"`

	// Expires is when this message must be forgotten (disappearing messages);
	// zero means it is kept until pushed out. Never sent to the model.
	Expires time.Time `json:"-"`
}

// Completion is a model reply plus what it cost.
type Completion struct {
	Text         string
	InputTokens  int // all input tokens, cached ones included
	CachedTokens int // the part of InputTokens served from xAI's prompt cache
	OutputTokens int
	SearchCalls  int
	// Billed is the cost the provider reported, when it does (OpenRouter).
	Billed *float64
}

// searchCallUSD is xAI's price for one server-side web search call
// ($5 per 1,000), billed on top of tokens.
const searchCallUSD = 5.0 / 1000

// Cost is the request's price in USD: what the provider billed, else an
// estimate from the configured per-million token rates, billing cached input at its own lower rate. It is an
// estimate: long-prompt pricing tiers are not modelled.
func (c Completion) Cost(cfg *Config) float64 {
	if c.Billed != nil {
		return *c.Billed
	}
	cached := min(c.CachedTokens, c.InputTokens)
	return float64(c.InputTokens-cached)/1e6*cfg.PriceIn +
		float64(cached)/1e6*cfg.PriceCached +
		float64(c.OutputTokens)/1e6*cfg.PriceOut +
		float64(c.SearchCalls)*searchCallUSD
}

// Completer is a model API the bot can ask for a reply.
type Completer interface {
	Complete(ctx context.Context, system string, msgs []ChatMsg) (Completion, error)
}

// newHTTPClient is Go's default transport (proxy settings, timeouts, HTTP/2),
// but refusing anything older than TLS 1.3.
func newHTTPClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13}
	return &http.Client{Transport: t}
}

// apiErrorText is what an API error body says, for the log. Only its message
// is kept: other fields can quote the request (OpenRouter's moderation
// errors include the flagged input).
func apiErrorText(data []byte) string {
	var e struct {
		Message string          `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(data, &e) != nil {
		return "(unreadable error body)"
	}
	msg := e.Message
	var s string
	var o struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(e.Error, &s) == nil && s != "" {
		msg = s
	} else if json.Unmarshal(e.Error, &o) == nil && o.Message != "" {
		msg = o.Message
	}
	return cmp.Or(clamp(msg, 200), "(no message)")
}

// postJSON POSTs payload with a bearer key and returns the body of a 200
// response. A network error, 429 or 5xx gets one retry; name labels errors.
func postJSON(ctx context.Context, c *http.Client, name, url, key string, payload []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	const attempts = 2
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.Do(req)
		if err != nil {
			lastErr = err
		} else {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return data, nil
			}
			lastErr = fmt.Errorf("%s status %d: %s", name, resp.StatusCode, apiErrorText(data))
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

type XAI struct {
	cfg  *Config
	url  string
	http *http.Client
}

func NewXAI(cfg *Config) *XAI {
	return &XAI{cfg: cfg, url: "https://api.x.ai/v1/responses", http: newHTTPClient()}
}

// Complete sends the conversation to the x.ai Responses API and returns the reply text.
func (x *XAI) Complete(ctx context.Context, system string, msgs []ChatMsg) (Completion, error) {
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
	data, err := postJSON(ctx, x.http, "x.ai", x.url, x.cfg.APIKey, payload)
	if err != nil {
		return Completion{}, err
	}
	return parseOutput(data)
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
		return Completion{}, fmt.Errorf("malformed response") // not err: it can quote the body
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
		return c, fmt.Errorf("empty response") // not the body: it can hold the conversation
	}
	return c, nil
}

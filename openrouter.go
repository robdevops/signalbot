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
	CachedTokens int // the part of InputTokens served from the prompt cache
	OutputTokens int
	Cost         float64 // USD, as OpenRouter reports it; 0 if it did not
}

// Completer is a model API the bot can ask for a reply.
type Completer interface {
	Complete(ctx context.Context, system string, msgs []ChatMsg) (Completion, error)
}

// OpenRouter talks to OpenRouter's chat completions API.
type OpenRouter struct {
	cfg  *Config
	url  string
	http *http.Client
}

// NewOpenRouter uses Go's default transport (proxy settings, timeouts,
// HTTP/2), but refuses anything older than TLS 1.3.
func NewOpenRouter(cfg *Config) *OpenRouter {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS13}
	return &OpenRouter{cfg: cfg, url: "https://openrouter.ai/api/v1/chat/completions", http: &http.Client{Transport: t}}
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
				Content string `json:"content"`
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
	}
	if c.Text == "" {
		return c, fmt.Errorf("empty response") // not the body: it can hold the conversation
	}
	return c, nil
}

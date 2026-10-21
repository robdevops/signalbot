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

type XAI struct {
	cfg  *Config
	url  string
	http *http.Client
}

func NewXAI(cfg *Config) *XAI {
	return &XAI{
		cfg: cfg,
		url: "https://api.x.ai/v1/responses",
		http: &http.Client{Transport: &http.Transport{
			MaxIdleConns:        100,
			MaxIdleConnsPerHost: 100,
			IdleConnTimeout:     90 * time.Second,
			ForceAttemptHTTP2:   true,
		}},
	}
}

// Complete sends the conversation to the x.ai Responses API and returns the reply text.
func (x *XAI) Complete(ctx context.Context, msgs []ChatMsg) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	body := map[string]any{
		"model": x.cfg.Model,
		"input": append([]ChatMsg{{Role: "system", Content: x.cfg.SystemPrompt}}, msgs...),
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
		return "", err
	}

	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, x.url, bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+x.cfg.APIKey)
		req.Header.Set("Content-Type", "application/json")
		resp, err := x.http.Do(req)
		if err != nil {
			lastErr = err
		} else {
			data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
			resp.Body.Close()
			if resp.StatusCode == 429 || resp.StatusCode >= 500 {
				lastErr = fmt.Errorf("x.ai status %d: %s", resp.StatusCode, truncate(string(data), 200))
			} else if resp.StatusCode != 200 {
				return "", fmt.Errorf("x.ai status %d: %s", resp.StatusCode, truncate(string(data), 200))
			} else {
				return parseOutput(data)
			}
		}
		if ctx.Err() != nil {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	return "", lastErr
}

func parseOutput(data []byte) (string, error) {
	var r struct {
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, o := range r.Output {
		if o.Type != "message" {
			continue
		}
		for _, c := range o.Content {
			if c.Type == "output_text" || c.Type == "text" {
				sb.WriteString(c.Text)
			}
		}
	}
	out := strings.TrimSpace(sb.String())
	if out == "" {
		return "", fmt.Errorf("empty response: %s", truncate(string(data), 200))
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

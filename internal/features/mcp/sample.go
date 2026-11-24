package mcp

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	ext "github.com/robdevops/signalbot/addon"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// A sampling request's limits: its messages, and their text and the
// system prompt's, in bytes, in all.
const (
	maxSampleMessages = 16
	maxSampleText     = 32 << 10
)

// sampleTimeout is how long the model has to answer a sampling request.
var sampleTimeout = 2 * time.Minute

// gate holds an addon's sampling requests from initialize on, until the
// addon is ready to answer them: until open, which gives the addon, or
// nil for none, when they are refused.
type gate struct {
	once  sync.Once
	ready chan struct{}
	a     *addon
}

// open lets the requests through to a, or refuses them if a is nil. Only
// the first call counts. A nil gate does nothing.
func (g *gate) open(a *addon) {
	if g == nil {
		return
	}
	g.once.Do(func() {
		g.a = a
		close(g.ready)
	})
}

// sample waits for the gate to open, then answers as the addon's sample.
func (g *gate) sample(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	select {
	case <-g.ready:
	case <-ctx.Done():
		return nil, &rpcError{Code: -32000, Message: "addon not ready yet"}
	}
	if g.a == nil {
		return nil, &rpcError{Code: -32000, Message: "the bot can't answer model requests from this server"}
	}
	return g.a.sample(ctx, params)
}

// sample answers the addon's sampling/createMessage, whose params are
// params: it asks the bot's model, charged to the chat the request names,
// which must be one the addon may post in now, and within the addon's
// hourly rate. Only text is taken. A refusal is a JSON-RPC error with a
// short message; only the outcome, the time, sizes and cost are logged,
// never the prompt or the answer.
func (a *addon) sample(ctx context.Context, params json.RawMessage) (any, *rpcError) {
	start := time.Now()
	res, size, why, rerr := a.ask(ctx, params)
	outcome := "ok"
	if rerr != nil {
		outcome = why
	}
	var tokens int
	var cost *float64
	if res != nil {
		tokens, cost = res.tokens, res.cost
	}
	line := fmt.Sprintf("MCP server %s: a model request: %s in %dms, %d bytes in, %d tokens", a.server, outcome, time.Since(start).Milliseconds(), size, tokens)
	if cost != nil {
		line += fmt.Sprintf(", $%.4f", *cost)
	}
	if rerr != nil {
		logx.Warnf("%s", line)
		return nil, rerr
	}
	logx.Infof("%s", line)
	return res.result, nil
}

// answer is what the model said, and what it cost.
type answer struct {
	result ext.SampleResult
	tokens int
	cost   *float64 // nil if not reported
}

// ask is sample's work: the answer, the size of the request's text, and
// for a refusal, why, for the log, and the error to send.
func (a *addon) ask(ctx context.Context, params json.RawMessage) (*answer, int, string, *rpcError) {
	invalid := func(why, msg string) (*answer, int, string, *rpcError) {
		return nil, 0, why, &rpcError{Code: -32602, Message: msg}
	}
	var req ext.SampleRequest
	if json.Unmarshal(params, &req) != nil {
		return invalid("malformed", "invalid params")
	}
	if a.cfg.Model == nil || a.host == nil || a.host.Info().Model == "" {
		return nil, 0, "not allowed", &rpcError{Code: -32601, Message: "method not supported"}
	}
	size := len(req.SystemPrompt)
	for _, m := range req.Messages {
		size += len(m.Content.Text)
	}
	switch {
	case len(req.Messages) == 0 || len(req.Messages) > maxSampleMessages:
		return invalid("refused: messages", fmt.Sprintf("from 1 to %d messages", maxSampleMessages))
	case size > maxSampleText:
		return invalid("refused: too long", fmt.Sprintf("at most %d KB of text", maxSampleText>>10))
	case req.MaxTokens < 1:
		return invalid("refused: maxTokens", "maxTokens must be at least 1")
	case req.Temperature != nil && (math.IsNaN(*req.Temperature) || *req.Temperature < 0 || *req.Temperature > 2):
		return invalid("refused: temperature", "temperature must be from 0 to 2")
	}
	msgs := make([]map[string]string, 0, len(req.Messages)+1)
	if req.SystemPrompt != "" {
		msgs = append(msgs, map[string]string{"role": "system", "content": req.SystemPrompt})
	}
	for _, m := range req.Messages {
		if m.Role != "user" && m.Role != "assistant" || m.Content.Type != "text" {
			return invalid("refused: not text", "only text messages, from user or assistant")
		}
		msgs = append(msgs, map[string]string{"role": m.Role, "content": m.Content.Text})
	}
	now := a.now()
	a.mu.Lock()
	l, ok := a.leases[req.Meta.Chat]
	a.mu.Unlock()
	// A lease's day, not announce's for good: announce opens no sampling.
	if req.Meta.Chat == "" || !ok || !now.Before(l.Expires) || !a.allows(l.Chat) {
		return nil, size, "refused: no chat", &rpcError{Code: -32602, Message: "_meta needs the chat that pays, one the addon may post in now"}
	}
	if !a.asks.allow("", now) {
		return nil, size, "refused: over the hourly rate", &rpcError{Code: -32000, Message: "too many requests this hour"}
	}
	body := map[string]any{"messages": msgs, "max_tokens": min(req.MaxTokens, a.cfg.Model.MaxTokens)}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	// The addon cancelling, or exiting, doesn't stop the call: one stopped
	// part way is billed by the provider but never charged to the chat, so
	// the call runs to its end, or to the bot's own limit, and is charged,
	// and the answer is dropped.
	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sampleTimeout)
	defer cancel()
	data, err := a.host.CallAPI(feature.ChargeTo(callCtx, l.Chat), "/chat/completions", body)
	if err != nil {
		why := "failed"
		switch {
		case ctx.Err() != nil:
			why = "cancelled by the addon"
		case callCtx.Err() != nil:
			why = "timed out"
		}
		return nil, size, why, &rpcError{Code: -32603, Message: "the model couldn't answer"}
	}
	var r struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content *string `json:"content"`
			} `json:"message"`
			FinishReason string    `json:"finish_reason"`
			Error        *apiError `json:"error"` // OpenRouter's, for a provider that failed part way
		} `json:"choices"`
		Error *apiError `json:"error"`
		Usage struct {
			Tokens int      `json:"completion_tokens"`
			Cost   *float64 `json:"cost"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &r); err != nil || len(r.Choices) == 0 || r.Choices[0].Message.Content == nil {
		why := "no answer"
		switch {
		case err != nil:
			why += " (unreadable)"
		case len(r.Choices) > 0 && r.Choices[0].Error != nil:
			why += " (" + r.Choices[0].Error.String() + ")"
		case r.Error != nil:
			why += " (" + r.Error.String() + ")"
		case len(r.Choices) == 0:
			why += " (no choices)"
		default:
			why += " (finish " + txt.Clamp(cmp.Or(r.Choices[0].FinishReason, "none"), 40) + ")"
		}
		return nil, size, why, &rpcError{Code: -32603, Message: "the model gave no answer"}
	}
	c := r.Choices[0]
	res := &answer{
		result: ext.SampleResult{
			Role:       "assistant",
			Content:    ext.Content{Type: "text", Text: *c.Message.Content},
			Model:      cmp.Or(r.Model, a.host.Info().Model),
			StopReason: stopReason(c.FinishReason),
		},
		tokens: r.Usage.Tokens,
		cost:   r.Usage.Cost,
	}
	if ctx.Err() != nil {
		return res, size, "cancelled by the addon", &rpcError{Code: -32603, Message: "cancelled"}
	}
	return res, size, "", nil
}

// apiError is an error the model's API puts in a 200 answer: its code
// and message, which say what failed, never what was asked.
type apiError struct {
	Code    any    `json:"code"`
	Message string `json:"message"`
}

// String is e's code and message, short, for the log.
func (e *apiError) String() string {
	if e.Code == nil {
		return txt.Clamp("error: "+strings.TrimSpace(e.Message), 120)
	}
	return txt.Clamp(strings.TrimSpace(fmt.Sprintf("error %v: %s", e.Code, e.Message)), 120)
}

// stopReason is a completion's finish_reason as MCP names it.
func stopReason(finish string) string {
	switch finish {
	case "stop":
		return "endTurn"
	case "length":
		return "maxTokens"
	}
	return txt.Clamp(finish, 40)
}

// Package llm is the core's view of a language model: messages, tools, and
// the loop that runs the tools a model calls. Providers are adapters that
// implement Model.
package llm

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/img"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

// Message is one turn of a conversation.
type Message struct {
	Role       string // "system", "user", "assistant" or "tool"
	Content    string
	ToolCalls  []ToolCall // an assistant turn that calls tools
	ToolCallID string     // a tool's answer to that call
	Images     []string   // data: URLs shown to the model with Content
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

// Request is one reply to get: the system prompt, the conversation, the
// tools offered, and a tool the model must call first ("" for none).
type Request struct {
	System   string
	Messages []Message
	Tools    []feature.Tool
	Force    string
}

// Completion is a reply plus what it cost.
type Completion struct {
	Text         string
	InputTokens  int // all input tokens, cached ones included
	CachedTokens int // the part of InputTokens served from the prompt cache
	OutputTokens int
	Cost         float64 // USD, as the provider reports it; 0 if it did not
	ToolCalls    []ToolCall
	Images       []chat.Picture // pictures the tools made, to send with the reply
	Used         []string       // tools that ran without error, by name
}

// Turn is one request to a provider.
type Turn struct {
	Messages []Message
	Tools    []any  // tool entries for the request
	Force    string // a tool the model must call now, "" for none
}

// Model is a provider: one request, one completion.
type Model interface {
	Chat(ctx context.Context, t Turn) (Completion, error)
}

// Completer gets a whole reply, tool calls included.
type Completer interface {
	Complete(ctx context.Context, r Request) (Completion, error)
}

const (
	// MaxRounds caps the model requests one reply may take when the model
	// keeps calling tools.
	MaxRounds = 5
	// Budget bounds one whole reply, tool rounds included.
	Budget = 3 * time.Minute
)

// Loop is a Completer over a Model: when the model calls tools, it runs
// them and asks again, up to MaxRounds requests. Usage and cost are summed
// over all of them.
type Loop struct{ Model Model }

func (l Loop) Complete(ctx context.Context, r Request) (Completion, error) {
	ctx, cancel := context.WithTimeout(ctx, Budget)
	defer cancel()
	msgs := append([]Message{{Role: "system", Content: r.System}}, r.Messages...)
	defs := make([]any, len(r.Tools))
	for i, t := range r.Tools {
		defs[i] = t.Def
	}
	var total Completion
	for round := 1; ; round++ {
		turn := Turn{Messages: msgs, Tools: defs}
		if round == 1 {
			turn.Force = r.Force // required until its result is in
		}
		c, err := l.Model.Chat(ctx, turn)
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
		if round == MaxRounds {
			logx.Warnf("model still calling tools after %d requests; stopping", round)
			total.Text = cmp.Or(c.Text, "Sorry, I couldn't finish that.")
			return total, nil
		}
		msgs = append(msgs, Message{Role: "assistant", Content: c.Text, ToolCalls: c.ToolCalls})
		for _, tc := range c.ToolCalls {
			start := time.Now()
			res, label, err := run(ctx, r.Tools, tc)
			if err != nil {
				logx.Warnf("tool %s failed after %.1fs: %v", label, time.Since(start).Seconds(), err)
				logx.Debugf("tool %s arguments were %d bytes", label, len(tc.Function.Arguments))
			} else {
				logx.Infof("tool %s %.1fs", label, time.Since(start).Seconds())
				total.Used = append(total.Used, tc.Function.Name)
			}
			for _, pic := range res.Images {
				if len(total.Images) < img.MaxPerMessage {
					total.Images = append(total.Images, pic)
				}
			}
			msgs = append(msgs, Message{Role: "tool", ToolCallID: tc.ID, Content: res.Text})
		}
	}
}

// run runs one tool call, and names the tool for the log.
func run(ctx context.Context, tools []feature.Tool, tc ToolCall) (feature.Result, string, error) {
	i := slices.IndexFunc(tools, func(t feature.Tool) bool { return t.Name == tc.Function.Name && t.Run != nil })
	if i < 0 {
		return feature.Result{Text: "error: there is no tool named " + tc.Function.Name}, "unknown", errors.New("unknown tool")
	}
	res, err := tools[i].Run(ctx, tc.Function.Arguments)
	return res, tools[i].Label, err
}

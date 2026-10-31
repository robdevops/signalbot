// Package llm is the core's view of a language model: messages, tools, and
// the loop that runs the tools a model calls. Providers are adapters that
// implement Model.
package llm

import (
	"context"

	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/feature"
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

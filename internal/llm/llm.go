// Package llm is the core's view of a language model: messages, tools, and
// the loop that runs the tools a model calls. Providers are adapters that
// implement Model.
package llm

import "context"

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

// Image is a picture a tool made, to send with the reply.
type Image struct {
	Data    []byte
	Caption string // e.g. a chart's title; "" for none
}

// Result is what a tool returns: text for the model, and pictures for the
// reply.
type Result struct {
	Text   string
	Images []Image
}

// Tool is one tool offered to the model. Def is its entry in the request.
// Run is nil for a provider tool, which the provider runs itself (web search).
type Tool struct {
	Name  string // as the model calls it
	Label string // for the log
	Def   any
	Run   func(ctx context.Context, arguments string) (Result, error)
}

// Request is one reply to get: the system prompt, the conversation, the
// tools offered, and a tool the model must call first ("" for none).
type Request struct {
	System   string
	Messages []Message
	Tools    []Tool
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
	Images       []Image  // pictures the tools made, to send with the reply
	Used         []string // tools that ran without error, by name
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

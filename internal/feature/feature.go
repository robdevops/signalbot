// Package feature is the only part of the core a feature sees: the Host it
// registers with, and the types its hooks take and return. Features import
// this, the neutral chat types and kit, and nothing else of the bot.
package feature

import (
	"context"
	"time"

	"github.com/robdevops/signalbot/internal/chat"
)

// Feature is one optional part of the bot. Register is called once at
// startup, after the model's capabilities are known; an error stops the bot.
type Feature interface {
	Name() string
	Register(h Host) error
}

// Host is what a feature may use and hook into.
type Host interface {
	Context() context.Context // ends when the bot shuts down
	Env() Env
	Caps() Caps
	Info() Info

	// Command answers "/name ...". run gets the text after the name, and
	// reports false to let the message go to the model as usual. A name
	// already taken is an error.
	Command(name string, run func(ctx context.Context, t Turn, rest string) (Command, bool)) error
	// Tools offers a group of tools to the model, on the turns Offer allows.
	Tools(g ToolGroup)
	// OnReply may replace what is sent: nil keeps the reply.
	OnReply(func(t Turn, r Reply) *chat.Outgoing)
	// OnUnaddressed sees group messages not for the bot. Their text is
	// never logged.
	OnUnaddressed(func(m chat.Message))
	// OnForget runs when a chat's history is erased, so state about it goes.
	OnForget(func(c chat.ChatID))
	// Remember adds text to a chat's history, kept like any message:
	// HISTORY_TURNS, a day at most, sooner with the disappearing timer.
	Remember(c chat.ChatID, text string, expires time.Duration)
}

// Env reads the settings a feature owns. Every name read is recorded, so a
// test can check the README documents it.
type Env interface {
	String(name, def string) string
	Bool(name string, def bool) bool
	Int(name string, def int) int
}

// Caps is what the model and provider can do.
type Caps struct {
	Vision bool // the model accepts images
	Tools  bool // the model can call tools
}

// Info describes the bot, for features that talk about it.
type Info struct {
	Model string // the model's name without its provider, e.g. "deepseek-v4.1-flash-1017"
	Tools string // the tool groups by name, e.g. "web search, charts", or "none"
}

// Turn is one message the bot is answering. It is read-only.
type Turn struct {
	Message chat.Message
	Bot     string   // the bot's name in this chat
	Forced  string   // a chat command's tool, which the model must call; "" if none
	Offered []string // tools offered this turn, by name
}

// Command is what a command handler decided: Text is sent as the bot's own
// reply, without the model; or Tool names a tool the model must call.
type Command struct {
	Text string
	Tool string
}

// ToolGroup is tools offered together, and how the bot names them.
type ToolGroup struct {
	Prompt  string // in the system prompt's tools line, e.g. "charts (make_chart, make_donut_chart)"
	Intro   string // in {tools} and /example, e.g. "charts"
	Grouped bool   // the prompt lists tools by server: ask the model to name them that way
	Tools   []Tool
	Offer   func(t Turn) bool // nil: every turn
}

// Tool is one tool. Def is its entry in the model request; Run is nil for a
// provider tool, which the provider runs itself.
type Tool struct {
	Name  string // as the model calls it
	Label string // for the log
	Def   any
	Run   func(ctx context.Context, arguments string) (Result, error)
}

// Result is what a tool returns: text for the model, pictures for the reply.
type Result struct {
	Text   string
	Images []chat.Picture
}

// Reply is the model's answer to a turn, before it is sent.
type Reply struct {
	Text     string
	Pictures []chat.Picture
	Used     []string // tools that ran without error, by name
}

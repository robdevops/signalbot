// Package feature is the only part of the core that features use: the types
// shared by the core, features and the chat network, the Host a feature
// registers its hooks on, and the command helper.
package feature

import (
	"context"
	"encoding/json"
	"regexp"
	"time"
)

// MaxPictures caps a message's images, besides its sticker, and a reply's
// pictures.
const MaxPictures = 4

// Chat is a conversation: a group, or a DM with one user.
type Chat struct {
	GroupID string // set for a group
	User    string // in a DM, the other person: the network's ID for them
}

// Key names the chat in the core's maps.
func (c Chat) Key() string {
	if c.GroupID != "" {
		return "g:" + c.GroupID
	}
	return "u:" + c.User
}

// Message is a message posted in a chat.
type Message struct {
	Chat      Chat
	Sender    string        // the network's ID for the sender
	Name      string        // the sender's display name, made safe; "" if unknown
	Text      string        // the text, without the bot's @mention, other mentions as @Name or [unnamed user]
	Quote     *Quote        // the message it replies to, if any
	Media     []Media       // its images, then the quoted message's, at most MaxPictures; then its sticker
	Expires   time.Duration // the chat's disappearing-messages timer; 0 if off
	Addressed bool          // it mentions the bot, replies to it, or is a DM from an allowed sender
}

// Quote is the message another one replies to, as the replier's app sent it.
type Quote struct {
	Text   string // as the replier sent it, so not always what the author wrote
	Author string // the author's name, made safe; "" if unknown
	Bot    bool   // the bot wrote it
}

// Media is an image a message carries, or its sticker.
type Media struct {
	ID      string // the network's ID for it
	Quoted  bool   // a thumbnail of an image in the quoted message
	Sticker bool   // a sticker, not an image
	Label   string // a sticker's emoji and pack title, e.g. `: 🙂 from "Pack"`; filled in by the network
}

// Action is what an OnMessage hook decides. The zero value carries on.
type Action struct {
	Answer     string // reply with this text, without the model (addressed messages only)
	ToolChoice string // make the model call this tool before answering (addressed messages only)
	Context    string // keep this text in the chat's history (unaddressed messages only)
}

// For is a with only what counts for m: Answer and ToolChoice on an
// addressed message, Context on an unaddressed one.
func (a Action) For(m Message) Action {
	if m.Addressed {
		a.Context = ""
	} else {
		a.Answer, a.ToolChoice = "", ""
	}
	return a
}

// Additions is what an OnTurn hook adds to the model's request.
type Additions struct {
	Note        string   // a line added to the message
	HistoryNote string   // the line history keeps in Note's place; "" keeps Note
	Images      []string // data: URLs shown to the model with the message
}

// Picture is an image sent with a reply.
type Picture struct {
	Data        []byte
	Caption     string // sent in place of the reply's text
	HistoryNote string // what history keeps of it; "" keeps "[sent an image]"
}

// ToolGroup is a set of tools offered to the model together.
type ToolGroup struct {
	Names    []string // what it adds to the intro's {tools}
	Listing  string   // its item in the system prompt's tools line
	System   string   // a system-prompt line while it is offered
	Tools    []Tool
	Keywords *regexp.Regexp // if set, offered only when a message matches, and for 2 replies after
}

// Tool is a function tool, run by the bot (Schema and Run), or a server tool
// run by the model's provider itself (Server only, as "<provider>:<name>",
// such as "openrouter:<name>"). A server tool is offered only when the
// model's API is that provider's.
type Tool struct {
	Name, Description string
	Schema            json.RawMessage
	Server            string
	Run               func(ctx context.Context, args string) (string, []Picture, error)
}

// Info is what the core knows about itself. All but Name and Tools are
// known when features register.
type Info struct {
	Name       string   // the bot's name
	Version    string   // the bot's version; "" if unknown
	Model      string   // the model's ID, such as "provider/model" on OpenRouter; "" with no model
	Images     bool     // the model takes images
	CallsTools bool     // the model can call tools
	Tools      []string // every tool group's Names in feature order; none when it can't call tools
	Intro      string   // INTRO with {name}, {model} and {tools} filled in
}

// Group is a group chat.
type Group struct {
	ID, Name string
}

// Host is the core as features see it. Hooks run in feature order. A Host
// must be a pointer, since the command helper keys its names by it.
type Host interface {
	// OnMessage sees every message. For an unaddressed one it runs on the
	// reader goroutine, so it must not block or call the network. The first
	// non-zero Action wins.
	OnMessage(func(context.Context, Message) Action)
	// OnTurn adds to the model's request for the addressed message being
	// answered.
	OnTurn(func(context.Context, Message) Additions)
	// Tools offers a group of tools to the model.
	Tools(ToolGroup)
	// IntroLine adds a paragraph to the bot's introduction, where INTRO has
	// {more}, such as a pointer to a command.
	IntroLine(line string)
	// At runs fn once at t, however late, passing the time it was due. It
	// fails when too many timers are set.
	At(name string, t time.Time, fn func(ctx context.Context, due time.Time)) (cancel func(), err error)

	// Env reads a feature's own setting. The docs test finds each name
	// passed to it.
	Env(key, def string) string
	// Info says what the bot is.
	Info() Info
	// Media fetches the bytes of msg.Media[i].
	Media(ctx context.Context, msg Message, i int) ([]byte, error)
	// Deliver acts on msg as if its sender had just posted it in its chat.
	Deliver(msg Message)
	// Groups lists the groups the bot shares with user.
	Groups(ctx context.Context, user string) ([]Group, error)
	// Redact replaces every name and ID of anyone but the bot in text with
	// "redacted", for logging text the bot doesn't control.
	Redact(text string) string
	// Go runs fn for as long as the bot runs: its ctx ends when the bot
	// stops, which then waits for fn to return.
	Go(fn func(ctx context.Context))
}

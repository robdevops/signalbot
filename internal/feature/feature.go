// Package feature is the only part of the core that features use: the types
// shared by the core and features, the Host a feature registers its hooks
// on, and the command helper.
package feature

import (
	"context"
	"encoding/json"
	"regexp"
	"time"
)

// MaxPictures caps the pictures in a message, in or out.
const MaxPictures = 4

// Chat is a conversation: a group, or a DM with one user.
type Chat struct {
	GroupID string // set for a group
	User    string // in a DM, the other person: an ACI, else a phone number
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
	Sender    string        // the sender's ID: an ACI, else a phone number
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
	ID      string // signal-cli's attachment ID; "" for a sticker
	Type    string // an image's MIME type; "" for a sticker
	Quoted  bool   // a thumbnail of an image in the quoted message
	Pack    string // a sticker's pack ID; "" for an image
	Sticker int    // a sticker's index in its pack
	Label   string // a sticker's emoji and pack title, e.g. `: 🙂 from "Pack"`; filled in by the core
}

// Turn is the message being answered, in its chat.
type Turn struct {
	Message
}

// Action is what an OnMessage hook decides. The zero value carries on.
type Action struct {
	Ignore     bool   // drop the message
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
	System      string   // a system-prompt line
	Note        string   // a line added to the message
	HistoryNote string   // the line history keeps in Note's place; "" keeps Note
	Images      []string // data: URLs shown to the model with the message
}

// Reply is the model's answer as it is about to be sent.
type Reply struct {
	Text      string
	Pictures  []Picture
	ToolsUsed []string // the tools the model ran, as the tool loop reports them
}

// Picture is an image sent with a reply.
type Picture struct {
	Data        []byte
	Caption     string // sent as the text when a reply has none
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
// run by OpenRouter itself (Server only, such as "openrouter:<name>").
type Tool struct {
	Name, Description string
	Schema            json.RawMessage
	Server            string
	Run               func(ctx context.Context, args string) (string, []Picture, error)
}

// Info is what the core knows about itself in a chat.
type Info struct {
	Name       string   // the bot's name in the chat
	Model      string   // the model's OpenRouter ID, as "provider/model"
	Images     bool     // the model takes images
	CallsTools bool     // the model can call tools
	Tools      []string // every tool group's Names in feature order; none when it can't call tools
}

// Group is a Signal group.
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
	// OnTurn adds to the model's request for an addressed message.
	OnTurn(func(context.Context, Turn) Additions)
	// OnReply may change a reply. It runs only after the model replies.
	OnReply(func(Turn, *Reply))
	// Tools offers a group of tools to the model.
	Tools(ToolGroup)
	// At runs fn once at t, however late, passing the time it was due. It
	// fails when too many timers are set.
	At(name string, t time.Time, fn func(ctx context.Context, due time.Time)) (cancel func(), err error)

	// Env reads a feature's own setting, recorded for the docs test.
	Env(key, def string) string
	// Info says what the bot is in a chat.
	Info(Chat) Info
	// Media fetches the bytes of msg.Media[i].
	Media(ctx context.Context, msg Message, i int) ([]byte, error)
	// Deliver acts on msg as if its sender had just posted it in its chat.
	Deliver(msg Message)
	// Groups lists the groups the bot shares with user.
	Groups(ctx context.Context, user string) ([]Group, error)
}

// Package chat is the neutral model of a chat network that the core works
// with: messages in, replies out, and the Transport interface an adapter
// (Signal, and later others) implements.
package chat

import (
	"context"
	"strings"
	"time"
)

// ChatID names one conversation: "<transport>:<kind>:<id>", e.g.
// "signal:g:…" for a group or "signal:u:…" for a DM. The prefix routes
// replies to the transport that owns the chat.
type ChatID string

// Transport returns the transport kind that owns the chat, e.g. "signal".
func (c ChatID) Transport() string { k, _, _ := strings.Cut(string(c), ":"); return k }

// Person is a message's sender. Name is already safe to show the model, and
// "" when unknown.
type Person struct {
	ID   string
	Name string
}

// Quote is the message a message replies to.
type Quote struct {
	Text    string // as the sender's client quoted it: untrusted
	Author  string // display name, "" if unknown
	FromBot bool   // the quoted message is attributed to the bot
}

// MediaKind says what a Media is.
type MediaKind int

const (
	Image MediaKind = iota
	Sticker
)

// Media is an image or sticker in a message, fetched with Transport.Fetch.
type Media struct {
	ID    string // opaque to the core
	Kind  MediaKind
	Label string // for a sticker: e.g. `: 🙂 from "Pack"`, "" if unknown
}

// Message is a message ready for the core.
type Message struct {
	Chat    ChatID
	Group   bool
	Sender  Person
	Text    string        // mentions shown as "@Name", the bot's own removed
	Why     string        // why it is addressed: "mention", "reply" or "dm"; "" if not
	Quote   *Quote        // nil if not a reply
	Media   []Media       // images, then a sticker
	Expires time.Duration // the chat's disappearing-messages timer, 0 if off
}

// Incoming is a message as it arrives: what can be judged cheaply on the
// reader goroutine, and Load, which builds the full Message once the core
// has given it a slot. Load may call the network, and reports false when
// nothing is left to answer.
type Incoming struct {
	Chat      ChatID
	Group     bool
	Addressed bool
	Load      func(ctx context.Context) (Message, bool)
}

// Picture is an image to send, with its caption.
type Picture struct {
	Data    []byte
	Caption string
}

// Outgoing is a reply. Text may hold "@Name"; the transport turns names it
// knows in that chat into real mentions, and splits the text at its limit.
// Pictures go with the first part.
type Outgoing struct {
	Chat     ChatID
	Text     string
	Pictures []Picture
}

// Transport is a chat network adapter.
type Transport interface {
	Kind() string                                   // the ChatID prefix, e.g. "signal"
	Run(ctx context.Context, handle func(Incoming)) // reconnects itself; handle must not block
	Send(ctx context.Context, m Outgoing) error
	Typing(chat ChatID)
	Fetch(ctx context.Context, chat ChatID, m Media) ([]byte, error)
	Name(ctx context.Context) string // the bot's display name here, "" if unknown
	Describe() string                // prompt text: the network, markdown or not, message limit
	Forget(chat ChatID)              // drop per-chat state when the core erases the chat
}

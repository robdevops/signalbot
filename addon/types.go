// Package addon is signalbot's addon extension of MCP 2025-06-18: the wire
// format the bot and an addon share, and a ready stdio server for an addon
// written in Go. An addon is a local MCP server the bot runs as a child
// process (see docs/FILES_LINKS_TOOLS_MCP.md#addons): it answers its own
// commands without the model, reads the chat messages its watches match,
// and posts and reacts when it likes through its outbox.
//
// The bot's mcp feature and an addon both use these types, so the
// protocol is defined once. An addon in another language implements the
// same JSON, which schema.json, a JSON Schema kept in step with these types
// by a test, describes. The package uses only the standard library.
package addon

import (
	"encoding/json"
	"time"
)

// The protocol: the MCP version, the prefix of every key of the extension,
// the extension's version, and the outbox's URI.
const (
	Protocol  = "2025-06-18"
	Vendor    = "io.github.robdevops.signalbot/"
	Version   = 1
	OutboxURI = "signalbot://outbox"
)

// The extension's keys: the capability both sides offer in initialize, and
// the _meta keys on a call (the context), a tool (who may call it), a
// result (a watch to start, a reaction to put on the message) and a
// sampling request (the chat that pays).
const (
	KeyAddon      = Vendor + "addon"
	KeyContext    = Vendor + "context"
	KeyVisibility = Vendor + "visibility"
	KeyWatch      = Vendor + "watch"
	KeyReact      = Vendor + "react"
	KeyChat       = Vendor + "chat"
)

// Capability is the extension's entry in capabilities.experimental under
// KeyAddon. The bot offers its chat network and what it can do; an addon
// answers with the version alone.
type Capability struct {
	Version        int    `json:"version"`
	Network        string `json:"network,omitempty"`        // the bot's chat network, as it names itself: "signal", "telegram"
	WatchReplies   bool   `json:"watchReplies,omitempty"`   // the bot's: it takes a watch with Replies
	WatchReactions bool   `json:"watchReactions,omitempty"` // the bot's: it takes a watch with Reactions
}

// Context is the message a call the bot makes came from, in the call's
// _meta under KeyContext: pseudonyms for its chat and sender, a handle for
// it, and the bot's receive times in whole seconds, never the sender's
// clock.
type Context struct {
	Chat      string `json:"chat"`   // c_…, the chat's pseudonym
	Sender    string `json:"sender"` // u_…, the sender's, in that chat unless global_ids
	Ref       string `json:"ref"`    // r_…, the message, for a react op
	Addressed bool   `json:"addressed"`
	Group     bool   `json:"group"`
	Sent      int64  `json:"sent"`               // when the bot got it, in Unix seconds
	Edited    int64  `json:"edited"`             // when the bot got the edit; 0 for an original
	Name      string `json:"name,omitempty"`     // the sender's display name, with the names permission
	Deadline  int64  `json:"deadline,omitempty"` // a direct call's: when the bot stops waiting, in Unix seconds
}

// Watch is what a direct command's result asks for, in its _meta under
// KeyWatch: the messages matching Pattern in the command's chat, until
// Until, go to Tool instead of the model. With Replies, it takes only
// replies to the addon's own messages, and Pattern, which may then be
// empty, must match them too; offer it only to a bot whose Capability has
// WatchReplies.
type Watch struct {
	ID      string `json:"id"`
	Pattern string `json:"pattern,omitempty"` // RE2; needed unless Replies
	Until   string `json:"until"`             // RFC 3339
	Tool    string `json:"tool"`              // called with each message it matches, as {"text": …}
	Guard   string `json:"guard,omitempty"`   // the answer, while it lasts, to a message addressed to the bot that quotes the command's pictures
	Replies bool   `json:"replies,omitempty"` // only replies to the command's answer and the addon's posts in the chat while it lives; in a DM, every message
	// Reactions: also reactions put on, or taken off, the command's answer
	// and the addon's posts in the chat while it lives, each a call of Tool
	// with {"reaction": emoji, "removed": bool} and the reactor's context.
	// Offer it only to a bot whose Capability has WatchReactions.
	Reactions bool `json:"reactions,omitempty"`
	// Over is the answer, for an hour after the watch ends, to a message
	// addressed to the bot that replies to one of the addon's messages it
	// knew, in place of the model's: "That game is over."
	Over string `json:"over,omitempty"`
}

// Content is one block of a tool's result: text, or an image.
type Content struct {
	Type     string `json:"type"` // "text" or "image"
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"` // an image's bytes, in base64
	MimeType string `json:"mimeType,omitempty"`
}

// MarshalJSON writes a text block's text even when it is empty, as MCP's
// TextContent requires.
func (c Content) MarshalJSON() ([]byte, error) {
	type plain Content
	if c.Type != "text" {
		return json.Marshal(plain(c))
	}
	return json.Marshal(struct {
		plain
		Text string `json:"text"`
	}{plain(c), c.Text})
}

// Result is a tool's result.
type Result struct {
	Content []Content   `json:"content"`
	IsError bool        `json:"isError,omitempty"`
	Meta    *ResultMeta `json:"_meta,omitempty"`
}

// ResultMeta is what a result asks of the bot besides its content.
type ResultMeta struct {
	Watch *Watch `json:"io.github.robdevops.signalbot/watch,omitempty"` // a direct command's only
	React string `json:"io.github.robdevops.signalbot/react,omitempty"` // a watch's call's only: an emoji for the message it took
}

// Text is a result of words alone.
func Text(s string) Result { return Result{Content: []Content{{Type: "text", Text: s}}} }

// Op is one thing an addon's outbox asks the bot to do.
type Op struct {
	ID      string  `json:"id"`
	Op      string  `json:"op"`                // "post", "react" or "unwatch"
	Chat    string  `json:"chat,omitempty"`    // a post's or unwatch's chat pseudonym
	Text    string  `json:"text,omitempty"`    // a post's, with {u_…} placeholders the bot fills in with names
	Images  []Image `json:"images,omitempty"`  // a post's: at most 2, 2 MB in all
	Ref     string  `json:"ref,omitempty"`     // a reaction's message
	Emoji   string  `json:"emoji,omitempty"`   // a reaction's
	Watch   string  `json:"watch,omitempty"`   // the ID of the watch an unwatch ends
	ReplyTo string  `json:"replyTo,omitempty"` // a post's: the ID of an earlier post of the addon's in the same chat, which it is sent as a reply to
	Audio   *Audio  `json:"audio,omitempty"`   // a post's: one clip to play, MP3, at most 2 MB
}

// Audio is a clip in a post, sent to play, with no title of its own: on
// Telegram a voice message, on Signal an attachment named clip.mp3.
type Audio struct {
	Data     string `json:"data"`     // base64
	MimeType string `json:"mimeType"` // "audio/mpeg"
}

// Image is an image in a post.
type Image struct {
	Data     string `json:"data"` // base64
	MimeType string `json:"mimeType"`
	Name     string `json:"name,omitempty"`
}

// Page is what reading the outbox at OutboxURI + "/" + cursor returns.
type Page struct {
	Cursor string `json:"cursor"`
	More   bool   `json:"more"`
	Ops    []Op   `json:"ops"`
}

// Held is an op the bot hasn't read past yet, as an addon saves it to post
// it after a restart (Server.Outbox, Server.Restore).
type Held struct {
	Op     Op        `json:"op"`
	Queued time.Time `json:"queued"`
}

// SampleRequest is MCP's sampling/createMessage params, as an addon asks
// the bot's model (Server.Sample): the bot runs it on its own model and
// charges the chat in Meta. The bot takes text alone.
type SampleRequest struct {
	Messages     []SampleMessage `json:"messages"`
	SystemPrompt string          `json:"systemPrompt,omitempty"`
	MaxTokens    int             `json:"maxTokens"`             // the bot caps it at the addon block's max_tokens
	Temperature  *float64        `json:"temperature,omitempty"` // nil is the model's own
	Meta         SampleMeta      `json:"_meta"`
}

// SampleMeta is a sampling request's _meta.
type SampleMeta struct {
	Chat string `json:"io.github.robdevops.signalbot/chat"` // c_…, the chat that pays, one the addon may post in now
}

// SampleMessage is one message of a sampling request: what the user or the
// assistant said.
type SampleMessage struct {
	Role    string  `json:"role"` // "user" or "assistant"
	Content Content `json:"content"`
}

// SampleResult is the bot's answer to a sampling request: MCP's
// CreateMessageResult.
type SampleResult struct {
	Role       string  `json:"role"` // "assistant"
	Content    Content `json:"content"`
	Model      string  `json:"model"`                // the model that wrote it
	StopReason string  `json:"stopReason,omitempty"` // "endTurn", "maxTokens", …
}

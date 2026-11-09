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
// pictures; MaxFiles caps its other files.
const (
	MaxPictures = 4
	MaxFiles    = 4
)

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
	Media     []Media       // its images, then the quoted message's, at most MaxPictures; then its files and the quoted message's, at most MaxFiles; then its sticker
	Expires   time.Duration // the chat's disappearing-messages timer; 0 if off
	Timestamp int64         // when it was sent, by the sender's clock, as the network names it; 0 for one the bot made, such as a scheduled post. An edit keeps the original's
	Edited    int64         // for an edit of the message sent at Timestamp, which it replaces, when it was edited; 0 for an original
	Addressed bool          // it mentions the bot, replies to it, or is a DM from an allowed sender
}

// Quote is the message another one replies to, as the replier's app sent it.
type Quote struct {
	Text   string // as the replier sent it, so not always what the author wrote
	Author string // the author's name, made safe; "" if unknown
	Bot    bool   // the bot wrote it
	Sender string // the network's ID for its author; "" if unknown
	Sent   int64  // when it was first sent, as Message.Timestamp names it; 0 if unknown
}

// Media is an image a message carries, another file, or its sticker.
type Media struct {
	ID      string // the network's ID for it; "" for a quoted file it can't fetch
	Quoted  bool   // from the quoted message: an image's thumbnail, or the original image or file
	Sticker bool   // a sticker, not an image
	Label   string // a sticker's emoji and pack title, e.g. `: 🙂 from "Pack"`; filled in by the network
	File    bool   // a file that isn't an image: a document, audio, …
	Name    string // a file's name as its sender gave it, unsafe; "" if none
	MIME    string // a file's type as its sender's app gave it, unchecked
	Size    int    // a file's size in bytes, as its sender's app gave it
}

// Event is something that happened in a chat besides a message. Exactly
// one of its kinds is set.
type Event struct {
	Chat     Chat
	By       Person    // who reacted, or who changed the group; ID "" if unknown, or if several did
	Reaction *Reaction // a reaction to a message
	Members  *Members  // a change in a group's members
}

// Person is someone in a chat.
type Person struct {
	ID   string // the network's ID for them
	Name string // their display name, made safe; "" if unknown
}

// Reaction is an emoji put on a message, or taken back.
type Reaction struct {
	Emoji   string
	Removed bool   // the reaction was taken back
	Author  string // the network's ID for who wrote the message reacted to
	Bot     bool   // the bot wrote it
	Sent    int64  // its Timestamp, as Message.Timestamp names it
}

// Members is who joined and left a group, or that the bot was added to it.
type Members struct {
	Joined, Left []Person
	BotAdded     bool // the bot was just added: Joined and Left are empty
}

// File is a file shown to the model with a message: a PDF, or audio.
type File struct {
	Name, MIME string
	Data       []byte
}

// Action is what an OnMessage hook decides. The zero value carries on.
type Action struct {
	Answer     string // reply with this text, without the model (addressed messages only)
	ToolChoice string // make the model call this tool before answering (addressed messages only)
	Context    string // keep this text in the chat's history (unaddressed messages only)
}

// For returns a copy of the action keeping only the fields that apply to
// m: Answer and ToolChoice if m is addressed, Context if not.
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
	Files       []File   // files shown to the model with the message
	Content     bool     // Note or Files carry what a page or document says, written by whoever made it, not by the sender
}

// Picture is an image sent with a reply.
type Picture struct {
	Data        []byte
	Name        string // its file name without the extension, e.g. a title; "" lets the network choose
	Caption     string // sent in place of the reply's text
	HistoryNote string // what history keeps of it; "" keeps "[sent an image]"
}

// ToolGroup is a set of tools offered to the model together. One with no
// Tools, only Names, such as a feature's chat commands, is named only in
// the prompt's list of what the bot has, not in the intro's {tools}.
type ToolGroup struct {
	Names    []string // what it adds to the intro's {tools} and the prompt's list
	Listing  string   // its item in the system prompt's tools line
	System   string   // a system-prompt line while it is offered
	Tools    []Tool
	Keywords *regexp.Regexp // if set, offered only when a message matches, and for TOOL_LINGER_ROUNDS replies after (default 2)
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
// known when features register, though Images, Audio, PDFs and CallsTools turn off for
// the rest of the run if the model's API rejects them.
type Info struct {
	Name       string   // the bot's name
	Version    string   // the bot's version; "" if unknown
	Model      string   // the model's ID, such as "provider/model" on OpenRouter; "" with no model
	Images     bool     // the model takes images
	Audio      bool     // the model takes audio
	PDFs       bool     // the model's API takes PDFs
	CallsTools bool     // the model can call tools
	Tools      []string // the Names of every tool group with tools, in feature order; none when it can't call tools
	Intro      string   // INTRO with {name}, {model}, {tools} and {more} filled in, as in a group
	IntroDM    string   // the same in a DM, with IntroLine's DM-only lines too
}

// Store is a feature's saved state: one JSON document, written whole and
// atomically to <name>.json in the state directory, so a crash never leaves
// half of it.
type Store interface {
	// Load reads the saved document into v; found is false, with no error,
	// when nothing is saved yet. A file that can't be read is an error.
	Load(v any) (found bool, err error)
	// Save replaces the document with v. Over 4 MiB it fails and nothing is
	// written.
	Save(v any) error
}

// Group is a group chat.
type Group struct {
	ID, Name string
}

// Host is the core as features see it. Hooks run in feature order.
type Host interface {
	// OnMessage sees every message. For an unaddressed one it runs on the
	// reader goroutine, so it must not block or call the network. The first
	// non-zero Action wins.
	OnMessage(func(context.Context, Message) Action)
	// OnTurn adds to the model's request for the addressed message being
	// answered.
	OnTurn(func(context.Context, Message) Additions)
	// OnEvent sees each event on a slot of its own, so it may block or call
	// the network: a reaction in order with its chat's messages, a member
	// change once the network has worked it out, which can be after later
	// messages. The first non-empty reply returned is sent to the chat,
	// without the model.
	OnEvent(func(context.Context, Event) (reply string))
	// Command registers /name: an addressed message whose text is "/name"
	// or "/name args", the name in any case, gets fn's Action. A name is 1
	// to 32 lowercase letters, digits, _ or -, and is refused if another
	// feature has taken it.
	Command(name string, fn func(ctx context.Context, m Message, args string) Action) error
	// ListCommands names commands that use no model tool in the prompt's
	// list of what the bot has, as "**label**: /a, /b", so the model can
	// point a near-miss to them.
	ListCommands(label string, names ...string)
	// OfferTools offers a group of tools to the model, keeping only the
	// tools the model's API can run; kept is false if it can run none.
	OfferTools(ToolGroup) (kept bool)
	// IntroLine adds a paragraph to the bot's introduction, where INTRO has
	// {more}, such as a pointer to a command.
	IntroLine(line string)
	// IntroLineDM adds a paragraph to the bot's introduction in a DM only.
	IntroLineDM(line string)
	// At runs fn once at t, however late, passing the time it was due. It
	// fails when too many timers are set.
	At(name string, t time.Time, fn func(ctx context.Context, due time.Time)) (cancel func(), err error)

	// Env reads a feature's own setting. The docs test finds each name
	// passed to it.
	Env(key, def string) string
	// Store opens the feature's saved state, called name (lowercase
	// letters, digits, - and _, up to 32), while it registers. It fails,
	// stopping the bot, if the state directory can't be listed and written,
	// or another feature took the name.
	Store(name string) (Store, error)
	// Info says what the bot is.
	Info() Info
	// Media fetches the bytes of msg.Media[i].
	Media(ctx context.Context, msg Message, i int) ([]byte, error)
	// Deliver acts on msg as if its sender had just posted it in its chat.
	Deliver(msg Message)
	// Tell sends text from the bot to user in their DM, as written, never
	// through the model, if they may still DM the bot.
	Tell(user, text string)
	// Groups lists the groups the bot shares with user.
	Groups(ctx context.Context, user string) ([]Group, error)
	// Redact replaces every name and ID of anyone but the bot in text with
	// "redacted", for logging text the bot doesn't control.
	Redact(text string) string
	// Go runs fn for as long as the bot runs: its ctx ends when the bot
	// stops, which then waits for fn to return.
	Go(fn func(ctx context.Context))
}

// NotKept ends a history note for n things the model was shown but history
// doesn't keep, saying how it can be shown them again.
func NotKept(n int) string {
	if n == 1 {
		return "; it isn't kept, so to see it again they must reply to the message with it and mention you"
	}
	return "; they aren't kept, so to see them again they must reply to the message with them and mention you"
}

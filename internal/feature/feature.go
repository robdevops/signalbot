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

type chargeKey struct{}

// ChargeTo is ctx naming chat as the one that pays for the model's API
// calls made with it outside a tool, through Host.CallAPI.
func ChargeTo(ctx context.Context, chat Chat) context.Context {
	return context.WithValue(ctx, chargeKey{}, chat)
}

// ChargedChat is the chat ChargeTo named in ctx, if any.
func ChargedChat(ctx context.Context) (Chat, bool) {
	c, ok := ctx.Value(chargeKey{}).(Chat)
	return c, ok
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
	Addressed bool          // it mentions the bot, replies to it, starts (in a group) with one of the bot's commands, or is a DM from an allowed sender
	Bot       bool          // for React: the bot's own message, whose Timestamp an OnSent gave; Sender is unused
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
	Answer     string                           // reply with this text, without the model (addressed messages only)
	Pictures   []Picture                        // send these with Answer, or alone, without the model; captions as for a tool's pictures (addressed messages only)
	OnSent     func(sent []int64, dropped bool) // if set with Answer or Pictures, called once: with the timestamps they went out in, or dropped (addressed messages only); alone it doesn't count
	ToolChoice string                           // make the model call this tool before answering (addressed messages only)
	Context    string                           // keep this text in the chat's history (unaddressed messages only)
	Done       bool                             // the hook handled the message: no answer, no model, and for an unaddressed one nothing kept and no later hook
}

// IsZero reports whether a carries on: nothing set but, at most, OnSent,
// which only follows an Answer or Pictures.
func (a Action) IsZero() bool {
	return a.Answer == "" && len(a.Pictures) == 0 && a.ToolChoice == "" && a.Context == "" && !a.Done
}

// For returns a copy of the action keeping only the fields that apply to
// m: Answer, Pictures, OnSent and ToolChoice if m is addressed, Context if
// not, and Done either way.
func (a Action) For(m Message) Action {
	if m.Addressed {
		a.Context = ""
	} else {
		a.Answer, a.Pictures, a.OnSent, a.ToolChoice = "", nil, nil, ""
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

// Usage is what the model has cost: in one chat today, against the daily
// budget, and, when asked for, the API key's own totals.
type Usage struct {
	Tracked bool          // the core counts each chat's spending (DAILY_BUDGET is on); the chat's fields are zero otherwise
	Spent   float64       // US dollars this chat has spent today
	Limit   float64       // DAILY_BUDGET, in US dollars
	Resets  time.Duration // until midnight, local time, when the day's count starts again
	Key     *KeyUsage     // nil unless asked for and the API says
}

// KeyUsage is the API key's spending, as OpenRouter reports it, in US
// dollars.
type KeyUsage struct {
	Today, Week, Month, Total *float64 // nil for a period the API doesn't report
	Limit, Remaining          *float64 // the key's credit limit and what is left of it; nil without one
	Reset                     string   // how often the limit resets: "daily", "weekly", "monthly", or "" for never
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
	Provider   string   // the model API's provider, as server tools name it: "openrouter", or "" for another
	Network    string   // the chat network, as it names itself, such as "signal" or "telegram"
	Buttons    bool     // the network shows a post's Markup: buttons, a DM's keyboard, silent sends
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

// Post is what the bot posts on its own, from a timer or a worker.
type Post struct {
	Chat     Chat
	Label    string                           // history keeps the post as the bot's, as "[Label] text"; "" keeps nothing
	Text     string                           // Markdown, as written; @Name is linked to the chat's known names
	Pictures []Picture                        // sent with Text; captions as for a tool's pictures
	Audio    []byte                           // one MP3 clip sent with Text, as audio to play, with no title
	OnSent   func(sent []int64, dropped bool) // if set, called once: with the timestamps sent, or dropped
	ReplyTo  int64                            // if set, sent as a reply to the bot's own earlier message in Chat that an OnSent gave this timestamp; sent plainly if the network can't find it
	Markup   Markup                           // what it carries besides its content, where the network shows it
	// Edit, if set, makes the post an edit of the bot's own earlier message
	// in Chat that an OnSent gave this timestamp: its text, pictures and
	// buttons replaced, in place where the network can, else sent anew.
	// OnSent gets the timestamps that now stand for it; an edit the network
	// refused is sent, with none. History keeps nothing of it, and Label,
	// Audio and ReplyTo are unused.
	Edit int64
}

// Markup is what a post carries besides its content, on a network that
// shows it (Info.Buttons); another sends the post without it.
type Markup struct {
	Silent     bool       // sent without a notification
	Buttons    [][]Button // rows of buttons under the post's last message; a press is a Press
	Keyboard   [][]Key    // in a DM, the chat's reply keyboard from now on
	NoKeyboard bool       // in a DM, the reply keyboard taken away
}

// Button is a button under a post. Pressing it is a Press with its Data.
type Button struct {
	Label string
	Data  string // at most 64 bytes
}

// Key is a key of a DM's reply keyboard: a message of exactly its Label is
// handled as if Command, such as "/rain", had been sent.
type Key struct {
	Label, Command string
}

// Press is someone pressing a button under one of the bot's messages.
type Press struct {
	Chat Chat
	By   Person // who pressed it
	Sent int64  // the message the button is under, as an OnSent named it
	Data string // the button's Data
}

// Group is a group chat.
type Group struct {
	ID, Name string
}

// Host is the core as features see it. Hooks run in feature order.
type Host interface {
	// OnMessage sees every message. For an unaddressed one it runs on the
	// reader goroutine, so it must not block or call the network. The first
	// Action that isn't zero, counting only what applies to the message,
	// wins.
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
	// OnPress sees each press of a button under the bot's messages, in its
	// chat's slot, so it may block or call the network. The first hook
	// that takes it (ok) wins; its answer, if not "", is shown to the
	// presser for a moment.
	OnPress(func(context.Context, Press) (answer string, ok bool))
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
	// Post sends a post from the bot to its chat, as written, never through
	// the model, through the chat's mailbox, if the bot may still post
	// there.
	Post(p Post)
	// React puts emoji on msg in chat, as a reaction from the bot, through
	// the chat's mailbox at low priority: unless msg was deleted or edited
	// since, and unless the reaction is dropped to make room. chat mirrors
	// the network's React; msg's own Chat is ignored. A message the bot
	// made (Timestamp 0) can't be reacted to; nothing happens. One the bot
	// sent, with Bot set, can be.
	React(chat Chat, msg Message, emoji string)
	// Groups lists the groups the bot shares with user.
	Groups(ctx context.Context, user string) ([]Group, error)
	// CallAPI posts body as JSON to path (such as "/images" or
	// "/chat/completions") at the model's API and returns the response
	// body. Inside a tool's Run, what it cost counts toward the reply and
	// the chat's daily budget. Outside one, ctx must come from ChargeTo:
	// the core refuses the call when that chat is over its daily budget,
	// makes it, and adds what it cost to that chat's budget. Without
	// either, it refuses. For "/chat/completions" the core sets the model
	// to the bot's own, whatever the body says, and on OpenRouter asks for
	// the cost to be reported. The core adds the key, and on OpenRouter
	// asks for providers that don't collect the data; a feature never sees
	// the key.
	CallAPI(ctx context.Context, path string, body map[string]any) (json.RawMessage, error)
	// Usage is what the model has cost in chat today, and with key the API
	// key's own totals too, when the API reports them (OpenRouter's).
	Usage(ctx context.Context, chat Chat, key bool) Usage
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

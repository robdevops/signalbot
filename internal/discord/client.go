// Package discord is a chat network for the bot: a native client for
// Discord's gateway, over a WebSocket, and its REST API, that turns what it
// receives into messages for the core and sends replies in Discord's own
// Markdown. Only the main package uses it.
package discord

import (
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/kit/httpx"
)

// prefix starts every chat and user ID the client hands the core, so a bot
// on several networks knows a Discord chat by its ID alone.
const prefix = "discord:"

// Client is a Discord bot: its gateway session and its REST API.
type Client struct {
	base    string // the REST API's base URL
	token   string // never logged: scrub every error
	http    *http.Client
	backoff time.Duration // the first wait after a dropped session; a test's shorter
	fatal   time.Duration // the wait after Discord refuses the token or the intents
	typing  time.Duration // how often Typing is renewed; Discord's lasts 10s
	pending time.Duration // how long a slash command's "thinking…" waits for a reply

	self     atomic.Value // string: the bot's user ID
	username atomic.Value // string: its username
	name     atomic.Value // string: its display name
	app      atomic.Value // string: its application's ID
	known    chan struct{}
	once     sync.Once
	watch    atomic.Bool                   // the core has an event hook
	commands atomic.Pointer[[]bot.Command] // the commands features registered, for Discord's slash commands
	checked  atomic.Bool                   // the application's settings were checked
	listed   atomic.Bool                   // the slash commands were sent

	session   string // the gateway session to resume, and where; the reader goroutine's
	resumeURL string
	seq       atomic.Int64 // the last event's sequence number; 0 for none

	mu        sync.Mutex
	people    map[string]string                 // user ID to name, as last seen
	chats     map[string]map[string]string      // channel ID to the IDs and names seen there
	seenIn    map[string]map[string]bool        // user ID to the channels they were seen in
	channels  map[string]channel                // the guild channels the bot is in
	left      map[string]bool                   // channels the bot can no longer post in
	dms       map[string]string                 // user ID to their DM channel's ID
	dmUsers   map[string]string                 // a DM channel's ID to its user's ID
	authors   map[int64]string                  // a recent message's ID to its sender, the bot's own too
	authorQ   []int64                           // authors' keys, oldest first
	asked     map[string]map[int64]*interaction // slash commands waiting for their reply, by chat key and ID
	answering map[string]int64                  // the message the core is answering, by chat key
	botRoles  map[string]string                 // a guild's ID to the bot's own role there
	hooks     map[int64]*interaction            // a slash command's reply, by its message ID, for an edit

	test *config.Test // nil in production
}

// channel is a guild channel or thread the bot can see.
type channel struct {
	guild, name, guildName string
}

// New makes the client its settings say: DISCORD_BOT_TOKEN, the bot's token
// from Discord's developer portal, and DISCORD_API_URL, by default
// Discord's. The bot answers anyone who DMs it and every channel it is in:
// Discord's own settings decide who can reach it.
func New(test *config.Test) *Client {
	return newClient(config.Env("DISCORD_API_URL", "https://discord.com/api/v10"), config.Env("DISCORD_BOT_TOKEN", ""), test)
}

func newClient(base, token string, test *config.Test) *Client {
	return &Client{base: strings.TrimRight(base, "/"), token: token, http: httpx.Client(8),
		backoff: time.Second, fatal: 10 * time.Minute, typing: 8 * time.Second, pending: time.Minute, known: make(chan struct{}),
		people: map[string]string{}, chats: map[string]map[string]string{}, seenIn: map[string]map[string]bool{},
		channels: map[string]channel{}, left: map[string]bool{}, dms: map[string]string{}, dmUsers: map[string]string{},
		authors: map[int64]string{}, asked: map[string]map[int64]*interaction{},
		answering: map[string]int64{}, botRoles: map[string]string{}, hooks: map[int64]*interaction{},
		test: test}
}

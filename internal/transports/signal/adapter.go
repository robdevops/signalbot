package signal

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robdevops/signalbot/internal/chat"
)

// MaxChars is Signal's limit for one message; replies are split at it.
const MaxChars = 2000

// maxChats bounds the chats whose names are remembered: chat ids come from
// the network.
const maxChats = 500

// Adapter is the Signal network for the core: envelopes in as chat
// messages, replies out with real mentions.
type Adapter struct {
	c       *Client
	names   *Names
	allowed map[string]bool // senders who may DM the bot
	ctx     context.Context

	mu    sync.Mutex
	known map[chat.ChatID]map[string]string // chat -> name -> uuid shown to the model

	refreshPending atomic.Bool // a contact reload is already scheduled
}

// NewAdapter is the Signal network on the signal-cli socket at addr, with
// DMs allowed from the senders in allowed.
func NewAdapter(addr string, allowed map[string]bool) *Adapter {
	return &Adapter{c: New(addr), names: NewNames(), allowed: allowed, ctx: context.Background(),
		known: map[chat.ChatID]map[string]string{}}
}

// Client is the signal-cli client underneath.
func (a *Adapter) Client() *Client { return a.c }

func (a *Adapter) Kind() string { return "signal" }

// Run reads messages until ctx ends, reconnecting as needed, and loads the
// contact list after each connection.
func (a *Adapter) Run(ctx context.Context, handle func(chat.Incoming)) {
	a.ctx = ctx
	a.c.OnConnect = a.refreshSoon
	a.c.Run(ctx, func(env *Envelope) { a.inbound(env, handle) })
}

// Inject handles one envelope as if read from the socket: for tests.
func (a *Adapter) Inject(env *Envelope, handle func(chat.Incoming)) { a.inbound(env, handle) }

func userChat(id string) chat.ChatID  { return chat.ChatID("signal:u:" + id) }
func groupChat(id string) chat.ChatID { return chat.ChatID("signal:g:" + id) }

// target is where a chat's messages go.
func target(c chat.ChatID) Target {
	rest := strings.TrimPrefix(string(c), "signal:")
	if g, ok := strings.CutPrefix(rest, "g:"); ok {
		return Target{GroupID: g}
	}
	return Target{User: strings.TrimPrefix(rest, "u:")}
}

// refreshSoon reloads the contact list as soon as the rate limit allows. Calls
// made while a reload is already scheduled fold into it, so a burst of group
// changes costs one reload and at most one timer.
func (a *Adapter) refreshSoon() {
	if a.ctx.Err() != nil || !a.refreshPending.CompareAndSwap(false, true) {
		return
	}
	time.AfterFunc(a.names.untilRefreshAllowed(), func() {
		a.refreshPending.Store(false)
		if a.ctx.Err() == nil {
			a.refreshContacts(a.ctx)
		}
	})
}

// refreshContacts loads the contact list unless that was done moments ago,
// and reports whether it tried.
func (a *Adapter) refreshContacts(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return a.names.Refresh(ctx, a.c)
}

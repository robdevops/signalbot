package signal

import (
	"cmp"
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

const (
	// maxNamesAge is how old the contact list may get before the next
	// mention reloads it: it catches renames no group event reports.
	maxNamesAge = 24 * time.Hour
	// maxNames caps the names kept: IDs come from the network. A dropped
	// name is simply learned again.
	maxNames = 5000
	// DefaultName is what the bot calls itself without a profile name.
	DefaultName = "signalbot"
)

// Name ranks: a name is never replaced by one of lower rank.
const (
	rankUsername = 1
	rankSeen     = 2 // a sender's name on a live message
	rankGiven    = 3 // the person's own profile name
	rankContact  = 4 // the account owner's name for the contact
)

type rankedName struct {
	name string
	rank int
}

// names maps Signal IDs (ACI or number) to names, learned from senders and
// from signal-cli's contact list.
type names struct {
	mu          sync.Mutex
	byID        map[string]rankedName
	refreshedAt time.Time // when a load was last tried
	loadedAt    time.Time // when one last worked
	minRefresh  time.Duration
	loading     sync.Mutex  // held while the list loads, so a second caller waits for it
	pending     atomic.Bool // a reload is already scheduled
}

func newNames() names {
	return names{byID: map[string]rankedName{}, minRefresh: 15 * time.Second}
}

// contact is the part of a listContacts entry the bot uses.
type contact struct {
	Number    string `json:"number"`
	UUID      string `json:"uuid"`
	Username  string `json:"username"`
	GivenName string `json:"givenName"`
	Profile   struct {
		GivenName string `json:"givenName"`
	} `json:"profile"`
}

// best is the contact's name: the account owner's name for them, then their
// own profile name, then their username.
func (k contact) best() rankedName {
	for _, r := range []rankedName{{k.GivenName, rankContact}, {k.Profile.GivenName, rankGiven}, {k.Username, rankUsername}} {
		if r.name = txt.SafeName(r.name); r.name != "" {
			return r
		}
	}
	return rankedName{}
}

// set records a name for ids. The caller holds n.mu.
func (n *names) set(r rankedName, ids ...string) {
	for _, id := range ids {
		if id == "" || r.rank < n.byID[id].rank {
			continue
		}
		if _, ok := n.byID[id]; !ok && len(n.byID) >= maxNames {
			for k := range n.byID { // drop any one
				delete(n.byID, k)
				break
			}
		}
		n.byID[id] = r
	}
}

func (n *names) lookup(ids ...string) rankedName {
	n.mu.Lock()
	defer n.mu.Unlock()
	var best rankedName
	for _, id := range ids {
		if r := n.byID[id]; id != "" && r.rank > best.rank {
			best = r
		}
	}
	return best
}

// replace swaps every name for signal-cli's current list, so people who
// left their groups are forgotten.
func (n *names) replace(cs []contact) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.byID = map[string]rankedName{}
	for _, k := range cs {
		if r := k.best(); r.rank > 0 {
			n.set(r, k.UUID, k.Number)
		}
	}
	n.loadedAt = time.Now()
}

// Name is the best name known for a person by any of their IDs, made safe,
// or "".
func (c *Client) Name(ids ...string) string { return c.names.lookup(ids...).name }

// BotName is the bot's own profile name, or "signalbot" without one. It
// never blocks; LoadNames loads it.
func (c *Client) BotName() string { return cmp.Or(c.Name(c.ID()), DefaultName) }

// mentionName is the name signal-cli put in a mention, unless that is only
// the ID. The sender chose it, so it is made safe.
func mentionName(m Mention) string {
	if m.Name != m.UUID && m.Name != m.Number {
		return txt.SafeName(m.Name)
	}
	return ""
}

// resolve is the best name for a mention, or "": a contact, profile or
// sender name, then the name in the mention (unless it is an ID), then a
// username.
func (c *Client) resolve(m Mention) string {
	r := c.names.lookup(m.UUID, m.Number)
	if mn := mentionName(m); r.rank < rankSeen && mn != "" {
		return mn
	}
	return r.name
}

// stale reports whether the contact list should be loaded for a mention:
// it never was, it is over a day old, or it has no real name for them.
func (c *Client) stale(m Mention) bool {
	n := &c.names
	n.mu.Lock()
	never := n.refreshedAt.IsZero()
	old := !n.loadedAt.IsZero() && time.Since(n.loadedAt) > maxNamesAge
	n.mu.Unlock()
	return never || old || (n.lookup(m.UUID, m.Number).rank < rankSeen && mentionName(m) == "")
}

// LoadNames learns the sender's name from env, and loads the contact list
// if a mention of someone else needs it, or if the bot's own name is
// unknown. It may call signal-cli, so it runs in the message's slot.
func (c *Client) LoadNames(ctx context.Context, env *Envelope) {
	self := c.ID()
	if name := txt.SafeName(env.SourceName); name != "" {
		c.names.mu.Lock()
		c.names.set(rankedName{name, rankSeen}, env.SourceUUID, env.SourceNumber)
		c.names.mu.Unlock()
	}
	need := c.Name(self) == ""
	if d := env.DataMessage; d != nil {
		for _, m := range d.Mentions {
			need = need || (m.UUID != self && m.Number != self && c.stale(m))
		}
	}
	if need && c.refresh(ctx) && c.Name(self) == "" {
		logx.Warnf("own profile name not found; calling itself %s (INTRO can name it)", DefaultName)
	}
}

// refreshSoon loads the contact list as soon as the rate limit allows.
// Calls made while a load is scheduled fold into it, so a burst of group
// changes costs one load and one timer.
func (c *Client) refreshSoon() {
	n := &c.names
	if c.closed.Load() || !n.pending.CompareAndSwap(false, true) {
		return
	}
	n.mu.Lock()
	wait := max(0, n.minRefresh-time.Since(n.refreshedAt))
	n.mu.Unlock()
	time.AfterFunc(wait, func() {
		n.pending.Store(false)
		if !c.closed.Load() {
			c.refresh(context.Background())
		}
	})
}

// refresh loads the contact list, at most once per minRefresh, and reports
// whether it tried.
func (c *Client) refresh(ctx context.Context) bool {
	n := &c.names
	// A caller that arrives mid-load waits, then finds the list fresh.
	n.loading.Lock()
	defer n.loading.Unlock()
	n.mu.Lock()
	if time.Since(n.refreshedAt) < n.minRefresh {
		n.mu.Unlock()
		return false
	}
	n.refreshedAt = time.Now()
	n.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var cs []contact
	if err := c.call(ctx, "listContacts", map[string]any{"allRecipients": true}, &cs); err != nil {
		logx.Warnf("listContacts failed: %v", err)
		return true
	}
	logx.Debugf("loaded %d contacts", len(cs))
	n.replace(cs)
	return true
}

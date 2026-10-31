package main

import (
	"context"
	"sync"
	"time"

	"github.com/robdevops/signalbot/internal/kit/logx"

	"github.com/robdevops/signalbot/internal/signal"

	"github.com/robdevops/signalbot/internal/kit/txt"
)

// Name ranks: a name is never replaced by one of lower rank.
const (
	rankUsername = 1
	rankSeen     = 2 // sender name from a live message
	rankGiven    = 3 // profile given name
	rankContact  = 4 // the account owner's own name for the contact
)

// displayName prefers the given name (the account owner's contact name, then
// the person's own profile name), else the username.
func displayName(c signal.Contact) (name string, rank int) {
	for _, r := range []rankedName{{c.GivenName, rankContact}, {c.Profile.GivenName, rankGiven}, {c.Username, rankUsername}} {
		if name := txt.Safe(r.name); name != "" {
			return name, r.rank
		}
	}
	return "", 0
}

type rankedName struct {
	name string
	rank int
}

// Names maps Signal ids (uuid or number) to real names, learned from
// message senders and from signal-cli's contact list.
type Names struct {
	mu          sync.Mutex
	byID        map[string]rankedName
	refreshedAt time.Time // when a load was last attempted
	loadedAt    time.Time // when one last succeeded
	minRefresh  time.Duration
	loading     sync.Mutex // held while the contact list loads, so a second caller waits for it
}

func NewNames() *Names {
	return &Names{byID: map[string]rankedName{}, minRefresh: 15 * time.Second}
}

// maxNamesAge is how old the contact list may get before the next mention
// reloads it: it catches renames and edits that no group event reports.
const maxNamesAge = 24 * time.Hour

// maxNames bounds the name cache: ids come from the network. A dropped name
// is simply re-learned.
const maxNames = 5000

// set records a name for ids. Caller holds n.mu.
func (n *Names) set(name string, rank int, ids ...string) {
	for _, id := range ids {
		if id != "" && rank >= n.byID[id].rank {
			makeRoom(n.byID, id, maxNames)
			n.byID[id] = rankedName{name, rank}
		}
	}
}

// Learn records the sender's profile name from an incoming envelope.
func (n *Names) Learn(env *signal.Envelope) {
	if name := txt.Safe(env.SourceName); name != "" {
		n.mu.Lock()
		defer n.mu.Unlock()
		n.set(name, rankSeen, env.SourceUUID, env.SourceNumber)
	}
}

// replaceContacts swaps the whole cache for signal-cli's current list, so
// people who left their groups are forgotten.
func (n *Names) replaceContacts(cs []signal.Contact) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.byID = map[string]rankedName{}
	for _, c := range cs {
		if name, rank := displayName(c); rank > 0 {
			n.set(name, rank, c.UUID, c.Number)
		}
	}
	n.loadedAt = time.Now()
}

// untilRefreshAllowed is how long until Refresh may load the contact list.
func (n *Names) untilRefreshAllowed() time.Duration {
	n.mu.Lock()
	defer n.mu.Unlock()
	return max(0, n.minRefresh-time.Since(n.refreshedAt))
}

func (n *Names) lookup(ids ...string) rankedName {
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

// unnamedUser stands in for a mention whose name we could not find.
const unnamedUser = "[unnamed user]"

// realMentionName is the name signal-cli put in the mention, unless that is
// just the id. The sender chose it, so it is sanitised before use.
func realMentionName(m signal.Mention) string {
	if m.Name != m.UUID && m.Name != m.Number {
		return txt.Safe(m.Name)
	}
	return ""
}

// Resolve returns the best name for a mention, or "" if none is known: a
// contact, profile or sender name, then the name signal-cli put in the
// mention (unless it is just an id), then a username.
func (n *Names) Resolve(m signal.Mention) string {
	r := n.lookup(m.UUID, m.Number)
	if mn := realMentionName(m); r.rank < rankSeen && mn != "" {
		return mn
	}
	return r.name
}

// needsRefresh reports whether the contact list should be (re)loaded for this
// mention: we have never loaded it, it is over a day old, or we have no real
// name for the person.
func (n *Names) needsRefresh(m signal.Mention) bool {
	n.mu.Lock()
	never := n.refreshedAt.IsZero()
	stale := !n.loadedAt.IsZero() && time.Since(n.loadedAt) > maxNamesAge
	n.mu.Unlock()
	return never || stale || (n.lookup(m.UUID, m.Number).rank < rankSeen && realMentionName(m) == "")
}

// Refresh loads signal-cli contacts, at most once per minRefresh, and reports
// whether it tried.
func (n *Names) Refresh(ctx context.Context, s *signal.Client) bool {
	// A caller that arrives mid-load waits, and then finds the cache fresh.
	n.loading.Lock()
	defer n.loading.Unlock()
	n.mu.Lock()
	if time.Since(n.refreshedAt) < n.minRefresh {
		n.mu.Unlock()
		return false
	}
	n.refreshedAt = time.Now()
	n.mu.Unlock()

	cs, err := s.ListContacts(ctx)
	if err != nil {
		logx.Warnf("listContacts failed: %v", err)
		return true
	}
	logx.Debugf("loaded %d contacts", len(cs))
	n.replaceContacts(cs)
	return true
}

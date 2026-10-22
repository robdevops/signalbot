package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// Contact is the subset of a signal-cli listContacts entry we use.
type Contact struct {
	Number    string `json:"number"`
	UUID      string `json:"uuid"`
	Username  string `json:"username"`
	GivenName string `json:"givenName"`
	Profile   struct {
		GivenName string `json:"givenName"`
	} `json:"profile"`
}

// Name ranks: a higher rank is never replaced by a lower one. Only
// rankContact is trusted: every other source is a name its owner chose.
const (
	rankUsername = 1
	rankSeen     = 2 // sender name from a live message (self-chosen)
	rankGiven    = 3 // profile given name (self-chosen)
	rankContact  = 4 // the account owner's own name for the contact (trusted)
)

// DisplayName prefers the given name (the account owner's contact name, then
// the person's own profile name), else the username.
func (c Contact) DisplayName() (name string, rank int) {
	if g := strings.TrimSpace(c.GivenName); g != "" {
		return g, rankContact
	}
	if g := strings.TrimSpace(c.Profile.GivenName); g != "" {
		return g, rankGiven
	}
	if u := strings.TrimSpace(c.Username); u != "" {
		return u, rankUsername
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
	refreshedAt time.Time
	minRefresh  time.Duration
}

func NewNames() *Names {
	return &Names{byID: map[string]rankedName{}, minRefresh: 15 * time.Second}
}

// maxNames bounds the name cache. Ids come from the network, so it must not
// grow without limit; past the cap an arbitrary entry is dropped (map
// iteration order is random), and a dropped name is simply re-learned.
const maxNames = 5000

// set records a name for ids. Caller holds n.mu.
func (n *Names) set(name string, rank int, ids ...string) {
	for _, id := range ids {
		if id == "" || rank < n.byID[id].rank {
			continue
		}
		if _, ok := n.byID[id]; !ok && len(n.byID) >= maxNames {
			for k := range n.byID {
				delete(n.byID, k)
				break
			}
		}
		n.byID[id] = rankedName{name, rank}
	}
}

// Learn records the sender's profile name from an incoming envelope.
func (n *Names) Learn(env *Envelope) {
	name := safeName(env.SourceName)
	if n == nil || name == "" {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.set(name, rankSeen, env.SourceUUID, env.SourceNumber)
}

func (n *Names) addContacts(cs []Contact) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, c := range cs {
		if name, rank := c.DisplayName(); rank > 0 {
			if name = safeName(name); name != "" {
				n.set(name, rank, c.UUID, c.Number)
			}
		}
	}
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
func realMentionName(m Mention) string {
	if m.Name != m.UUID && m.Name != m.Number {
		return safeName(m.Name)
	}
	return ""
}

// ResolveRanked returns the best name for a mention and how much to trust it:
// given name, sender name, username, then the name signal-cli put in the
// mention (unless it is just an id), else unnamedUser. A miss is logged.
func (n *Names) ResolveRanked(m Mention) (string, int) {
	name, rank := n.resolve(m)
	if rank == 0 {
		id := m.UUID
		if id == "" {
			id = m.Number
		}
		logf("unresolved mention of %s", id)
	}
	return name, rank
}

// resolve is ResolveRanked without logging a miss.
func (n *Names) resolve(m Mention) (string, int) {
	var r rankedName
	if n != nil {
		r = n.lookup(m.UUID, m.Number)
	}
	switch {
	case r.rank >= rankSeen:
		return r.name, r.rank
	case realMentionName(m) != "":
		return realMentionName(m), rankSeen
	case r.rank > 0:
		return r.name, r.rank
	}
	return unnamedUser, 0
}

// Resolve is ResolveRanked without the rank.
func (n *Names) Resolve(m Mention) string {
	name, _ := n.ResolveRanked(m)
	return name
}

// needsRefresh reports whether the contact list should be (re)loaded for this mention:
// we have never loaded it, or we have no real name for the person.
func (n *Names) needsRefresh(m Mention) bool {
	if n == nil {
		return false
	}
	n.mu.Lock()
	never := n.refreshedAt.IsZero()
	n.mu.Unlock()
	return never || (n.lookup(m.UUID, m.Number).rank < rankSeen && realMentionName(m) == "")
}

// canRefresh reports whether Refresh would actually call signal-cli now,
// rather than return early because of the rate limit.
func (n *Names) canRefresh() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return time.Since(n.refreshedAt) >= n.minRefresh
}

// Refresh loads signal-cli contacts, at most once per minRefresh.
func (n *Names) Refresh(ctx context.Context, s *Signal) {
	n.mu.Lock()
	if time.Since(n.refreshedAt) < n.minRefresh {
		n.mu.Unlock()
		return
	}
	n.refreshedAt = time.Now()
	n.mu.Unlock()

	cs, err := s.ListContacts(ctx)
	if err != nil {
		logf("listContacts failed: %v", err)
		return
	}
	logf("loaded %d contacts", len(cs))
	n.addContacts(cs)
}

func (s *Signal) ListContacts(ctx context.Context) ([]Contact, error) {
	resp, err := s.call(ctx, "listContacts", map[string]any{"allRecipients": true})
	if err != nil {
		return nil, err
	}
	var cs []Contact
	if err := json.Unmarshal(resp.Result, &cs); err != nil {
		return nil, err
	}
	return cs, nil
}

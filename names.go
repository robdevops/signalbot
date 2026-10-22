package main

import (
	"context"
	"encoding/json"
	"log/slog"
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

// Name ranks: a higher rank is never replaced by a lower one.
const (
	rankUsername = 1
	rankSeen     = 2 // sender name from a live message
	rankGiven    = 3 // contact or profile given name
)

// DisplayName prefers the given name (contact, then profile), else the username.
func (c Contact) DisplayName() (name string, rank int) {
	for _, g := range []string{c.GivenName, c.Profile.GivenName} {
		if g = strings.TrimSpace(g); g != "" {
			return g, rankGiven
		}
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

func (n *Names) set(name string, rank int, ids ...string) {
	for _, id := range ids {
		if id != "" && rank >= n.byID[id].rank {
			n.byID[id] = rankedName{name, rank}
		}
	}
}

// Learn records the sender's profile name from an incoming envelope.
func (n *Names) Learn(env *Envelope) {
	if n == nil || strings.TrimSpace(env.SourceName) == "" {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.set(env.SourceName, rankSeen, env.SourceUUID, env.SourceNumber)
}

func (n *Names) addContacts(cs []Contact) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, c := range cs {
		if name, rank := c.DisplayName(); rank > 0 {
			n.set(name, rank, c.UUID, c.Number)
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

func realMentionName(m Mention) string {
	if m.Name != m.UUID && m.Name != m.Number {
		return m.Name
	}
	return ""
}

// Resolve returns the best name for a mention: given name, sender name, username,
// then the name signal-cli put in the mention (unless it is just an id), else unnamedUser.
func (n *Names) Resolve(m Mention) string {
	var r rankedName
	if n != nil {
		r = n.lookup(m.UUID, m.Number)
	}
	switch {
	case r.rank >= rankSeen:
		return r.name
	case realMentionName(m) != "":
		return realMentionName(m)
	case r.rank > 0:
		return r.name
	}
	slog.Warn("unresolved mention", "uuid", m.UUID, "number", m.Number, "mention_name", m.Name)
	return unnamedUser
}

// needsRefresh reports whether the contact list should be (re)loaded for this mention:
// we have never loaded it, or we have no real name for the person.
func (n *Names) needsRefresh(m Mention) bool {
	n.mu.Lock()
	never := n.refreshedAt.IsZero()
	n.mu.Unlock()
	return never || (n.lookup(m.UUID, m.Number).rank < rankSeen && realMentionName(m) == "")
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
		slog.Warn("listContacts failed", "err", err)
		return
	}
	slog.Info("loaded contacts", "count", len(cs))
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

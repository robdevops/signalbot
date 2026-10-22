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
	Number  string `json:"number"`
	UUID    string `json:"uuid"`
	Name    string `json:"name"`
	Profile struct {
		GivenName  string `json:"givenName"`
		FamilyName string `json:"familyName"`
	} `json:"profile"`
}

// DisplayName picks the best human name: contact name, then profile name.
func (c Contact) DisplayName() string {
	if n := strings.TrimSpace(c.Name); n != "" {
		return n
	}
	return strings.TrimSpace(c.Profile.GivenName + " " + c.Profile.FamilyName)
}

// Names maps Signal ids (uuid or number) to real names, learned from
// message senders and from signal-cli's contact list.
type Names struct {
	mu          sync.Mutex
	byID        map[string]string
	refreshedAt time.Time
	minRefresh  time.Duration
}

func NewNames() *Names {
	return &Names{byID: map[string]string{}, minRefresh: 15 * time.Second}
}

// Learn records the sender's profile name from an incoming envelope.
func (n *Names) Learn(env *Envelope) {
	if n == nil || strings.TrimSpace(env.SourceName) == "" {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, id := range []string{env.SourceUUID, env.SourceNumber} {
		if id != "" {
			n.byID[id] = env.SourceName
		}
	}
}

// addContacts fills in names without overwriting ones learned from live messages.
func (n *Names) addContacts(cs []Contact) {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, c := range cs {
		name := c.DisplayName()
		if name == "" {
			continue
		}
		for _, id := range []string{c.UUID, c.Number} {
			if id != "" && n.byID[id] == "" {
				n.byID[id] = name
			}
		}
	}
}

func (n *Names) lookup(ids ...string) string {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, id := range ids {
		if id != "" && n.byID[id] != "" {
			return n.byID[id]
		}
	}
	return ""
}

// Resolve returns the best name for a mention: known real name, then the name
// signal-cli put in the mention (unless it is just an id), else "someone".
func (n *Names) Resolve(m Mention) string {
	if n != nil {
		if name := n.lookup(m.UUID, m.Number); name != "" {
			return name
		}
	}
	if m.Name != "" && m.Name != m.UUID && m.Name != m.Number {
		return m.Name
	}
	return "someone"
}

// known reports whether we have a real name for the mention.
func (n *Names) known(m Mention) bool {
	return n.lookup(m.UUID, m.Number) != "" || (m.Name != "" && m.Name != m.UUID && m.Name != m.Number)
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
	n.addContacts(cs)
}

func (s *Signal) ListContacts(ctx context.Context) ([]Contact, error) {
	resp, err := s.call(ctx, "listContacts", nil)
	if err != nil {
		return nil, err
	}
	var cs []Contact
	if err := json.Unmarshal(resp.Result, &cs); err != nil {
		return nil, err
	}
	return cs, nil
}

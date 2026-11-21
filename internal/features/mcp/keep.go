package mcp

import (
	"cmp"
	"context"
	"maps"
	"regexp"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

// maxApplied is how many of an addon's last op IDs are kept, so an op read
// again is never applied twice.
const maxApplied = 1000

// addonState is what is saved of an addon: its live watches, the messages
// they took, the chats it may post in, and how far its outbox was read.
type addonState struct {
	Watches []watch          `json:"watches,omitempty"`
	Claims  []claim          `json:"claims,omitempty"`
	Leases  map[string]lease `json:"leases,omitempty"` // by chat pseudonym
	Cursor  string           `json:"cursor,omitempty"`
	Applied []string         `json:"applied,omitempty"` // oldest first
}

// keeper keeps the mcp store's document: the pseudonyms' secret and each
// addon's state. One goroutine saves it, whenever told something changed,
// so a message's hook never waits on the disk, and a burst of changes is
// one write.
type keeper struct {
	store  feature.Store
	doc    saved         // as loaded; an addon not running this time keeps its state here
	addons []*addon      // running, set before run starts
	dirty  chan struct{} // something changed
}

// newKeeper keeps doc, as loaded from store, for the addon servers named
// in servers; the state of any other is dropped at the first save.
func newKeeper(store feature.Store, doc saved, servers map[string]server) *keeper {
	maps.DeleteFunc(doc.Addons, func(name string, _ addonState) bool { return servers[name].Addon == nil })
	return &keeper{store: store, doc: doc, dirty: make(chan struct{}, 1)}
}

// changed marks the document changed, without waiting.
func (k *keeper) changed() {
	select {
	case k.dirty <- struct{}{}:
	default:
	}
}

// run saves the document each time it changes, and once more when ctx
// ends. A failed save is logged once, until one succeeds; the state stays
// in memory, and the next change tries again.
func (k *keeper) run(ctx context.Context) {
	failing := false
	for {
		var end bool
		select {
		case <-ctx.Done():
			end = true
		case <-k.dirty:
		}
		doc := saved{Secret: k.doc.Secret, Addons: maps.Clone(k.doc.Addons)}
		if doc.Addons == nil {
			doc.Addons = map[string]addonState{}
		}
		for _, a := range k.addons {
			doc.Addons[a.server] = a.state()
		}
		switch err := k.store.Save(doc); {
		case err != nil && !failing:
			logx.Warnf("addon state not saved, kept in memory until a save works: %v", err)
			failing = true
		case err == nil:
			failing = false
		}
		if end {
			return
		}
	}
}

// state is what is saved of the addon, without anything expired, which it
// forgets.
func (a *addon) state() addonState {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	s := addonState{Leases: map[string]lease{}, Cursor: a.cursor, Applied: slices.Clone(a.applied)}
	for key, ws := range a.watches {
		ws = slices.DeleteFunc(ws, func(w *watch) bool { return !now.Before(w.Until) })
		if len(ws) == 0 {
			delete(a.watches, key)
			continue
		}
		a.watches[key] = ws
		for _, w := range ws {
			c := *w
			c.Pictures = slices.Clone(w.Pictures)
			s.Watches = append(s.Watches, c)
		}
	}
	for k, c := range a.claims {
		if !now.Before(c.Expires) {
			delete(a.claims, k)
			continue
		}
		s.Claims = append(s.Claims, c)
	}
	for id, l := range a.leases {
		if !now.Before(l.Expires) {
			delete(a.leases, id)
			continue
		}
		s.Leases[id] = l
	}
	for id, t := range a.tombs {
		if !now.Before(t) {
			delete(a.tombs, id)
		}
	}
	// In a fixed order, so an unchanged state saves the same.
	slices.SortFunc(s.Watches, func(x, y watch) int {
		return cmp.Or(x.Until.Compare(y.Until), strings.Compare(x.Chat, y.Chat), strings.Compare(x.ID, y.ID))
	})
	slices.SortFunc(s.Claims, func(x, y claim) int { return x.Expires.Compare(y.Expires) })
	return s
}

// restore takes up s, as saved, leaving out what has expired, and a watch
// in a chat the addon is no longer allowed in or whose pattern no longer
// passes.
func (a *addon) restore(s addonState) {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for id, l := range s.Leases {
		if now.Before(l.Expires) {
			a.leases[id] = l
		}
	}
	for _, c := range s.Claims {
		if now.Before(c.Expires) {
			a.claims[c.claimKey] = c
		}
	}
	for _, w := range s.Watches {
		l, ok := a.leases[w.Chat]
		re, err := regexp.Compile(w.Pattern)
		if !ok || !now.Before(w.Until) || !a.allows(l.Chat) || err != nil || utf8.RuneCountInString(w.Pattern) > maxPattern || ordinary(re) ||
			len(a.watches[l.Chat.Key()]) >= maxWatches {
			continue
		}
		w.key, w.re = l.Chat.Key(), re
		a.watches[w.key] = append(a.watches[w.key], &w)
	}
	a.cursor = s.Cursor
	for _, id := range s.Applied[max(len(s.Applied)-maxApplied, 0):] {
		a.remember(id)
	}
}

// remember records that the op with id was applied, forgetting the oldest
// past maxApplied. a.mu is held.
func (a *addon) remember(id string) {
	if a.done[id] {
		return
	}
	a.done[id] = true
	a.applied = append(a.applied, id)
	if len(a.applied) > maxApplied {
		delete(a.done, a.applied[0])
		a.applied = slices.Delete(a.applied, 0, 1)
	}
}

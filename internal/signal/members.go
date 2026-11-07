package signal

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

// members keeps each group's members, as signal-cli's listGroups last gave
// them, so a group update, which says only that something changed, can be
// told apart into who joined and who left. It does nothing unless the core
// has a hook for events (Watch).
type members struct {
	c      *Client
	on     atomic.Bool // the core has an event hook
	mu     sync.Mutex
	known  map[string][]string // each group's members, by ACI else number; nil until first loaded
	by     map[string]*pending // the groups waiting to be compared
	busy   bool                // a comparison is set or running
	handle func(bot.Received)  // the core's, from Run
}

// pending is what is known of a group's updates since its last comparison.
type pending struct {
	by      feature.Person // who sent the update; ID "" once several are folded together
	updates int
	quiet   bool // every update was the bot's own: compared, but not reported
}

// Watch tells the client whether the core has a hook for events; without
// one it neither loads nor compares members.
func (c *Client) Watch(events bool) { c.members.on.Store(events) }

// start loads the groups' members once the bot's ID is known, so the first
// update is compared with them; nothing is reported for those already there.
func (m *members) start(ctx context.Context, handle func(bot.Received)) {
	m.mu.Lock()
	m.handle = handle
	loaded := m.known != nil
	m.mu.Unlock()
	if loaded || !m.on.Load() {
		return
	}
	if gs, ok := m.load(ctx, logx.Debugf); ok { // the first update loads them anyway
		m.mu.Lock()
		if m.known == nil {
			m.known = gs
		}
		m.mu.Unlock()
	}
}

// changed notes that group changed, sent by by (quiet if the bot itself),
// and compares its members soon, as often as the contact list may be
// loaded; a burst folds into one load.
func (m *members) changed(group string, by feature.Person, quiet bool) {
	if !m.on.Load() {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.by == nil {
		m.by = map[string]*pending{}
	}
	p := m.by[group]
	if p == nil {
		p = &pending{quiet: true}
		m.by[group] = p
	}
	if p.updates++; !quiet {
		if p.quiet || p.updates == 1 {
			p.by = by
		} else if p.by != by {
			p.by = feature.Person{} // several people: who did what isn't known
		}
		p.quiet = false
	}
	m.schedule()
}

// schedule sets a comparison, unless one is set or running. m.mu is held.
func (m *members) schedule() {
	if m.busy || m.c.closed.Load() {
		return
	}
	m.busy = true
	m.c.names.mu.Lock()
	wait := max(0, m.c.names.minRefresh-time.Since(m.c.names.refreshedAt))
	m.c.names.mu.Unlock()
	time.AfterFunc(wait, m.compare)
}

// compare loads the groups' members and hands the core an event for each
// group that changed: who joined and who left, or that the bot was added.
// The updates it compares are taken before listGroups is called, so one
// that arrives meanwhile waits for the next comparison; if the call fails,
// they are kept for it.
func (m *members) compare() {
	defer m.c.test.Begin()()
	m.mu.Lock()
	by := m.by
	m.by = nil
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	m.c.refresh(ctx) // names for newcomers, if the list may be loaded
	now, ok := m.load(ctx, logx.Warnf)
	m.mu.Lock()
	before, handle := maps.Clone(m.known), m.handle
	if ok && m.known == nil {
		m.known = now
	} else if ok { // only the groups compared: another group's change waits for its own comparison
		for g := range by {
			if ids, in := now[g]; in {
				m.known[g] = ids
			} else {
				delete(m.known, g)
			}
		}
	} else {
		for g, p := range by { // kept, under any that arrived since
			if m.by == nil {
				m.by = map[string]*pending{}
			}
			if _, newer := m.by[g]; !newer {
				m.by[g] = p
			}
		}
	}
	m.busy = false
	if len(m.by) > 0 && ok {
		m.schedule()
	}
	m.mu.Unlock()
	if !ok || handle == nil || m.c.closed.Load() {
		return
	}
	for group, p := range by {
		was, seen := before[group]
		is, still := now[group]
		var e feature.Members
		switch {
		case p.quiet || !still || before == nil: // the bot's own change, left by the bot, or nothing to compare with
			continue
		case !seen:
			e.BotAdded = true
		default:
			for _, id := range is {
				if !slices.Contains(was, id) {
					e.Joined = append(e.Joined, m.c.person(id, "", "", ""))
				}
			}
			for _, id := range was {
				if !slices.Contains(is, id) {
					e.Left = append(e.Left, m.c.person(id, "", "", ""))
				}
			}
			if len(e.Joined) == 0 && len(e.Left) == 0 {
				continue // its name or description changed
			}
		}
		chat := feature.Chat{GroupID: group}
		handle(bot.Received{Message: feature.Message{Chat: chat}, Event: &feature.Event{Chat: chat, By: p.by, Members: &e}})
	}
}

// load is every group the bot is in with its members, by ACI else number;
// a failure is logged with logf. Before the bot's ID is known it loads
// nothing, so the bot is never taken for a member.
func (m *members) load(ctx context.Context, logf func(string, ...any)) (map[string][]string, bool) {
	self := m.c.self()
	if self == "" {
		return nil, false
	}
	var gs []group
	if err := m.c.call(ctx, "listGroups", nil, &gs); err != nil {
		logf("listGroups failed: %v", err)
		return nil, false
	}
	out := map[string][]string{}
	for _, g := range gs {
		if !g.IsMember || g.ID == "" {
			continue
		}
		ids := []string{}
		for _, p := range g.Members {
			if id := cmp.Or(p.UUID, p.Number); id != "" && !is(self, p.UUID, p.Number) {
				ids = append(ids, id)
			}
		}
		out[g.ID] = ids
	}
	return out, true
}

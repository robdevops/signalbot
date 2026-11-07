package signal

import (
	"cmp"
	"context"
	"slices"
	"sync"
	"time"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

// members keeps each group's members, as signal-cli's listGroups last gave
// them, so a group update, which says only that something changed, can be
// told apart into who joined and who left.
type members struct {
	c      *Client
	mu     sync.Mutex
	known  map[string][]string       // each group's members, by ACI else number; nil until first loaded
	by     map[string]feature.Person // who changed each group waiting to be compared
	busy   bool                      // a comparison is waiting or running
	handle func(bot.Received)        // the core's, from Run
}

// start loads the groups' members, once the bot's ID is known, so the first
// update is compared with them; nothing is reported for those already there.
func (m *members) start(ctx context.Context, handle func(bot.Received)) {
	m.mu.Lock()
	m.handle = handle
	loaded := m.known != nil
	m.mu.Unlock()
	if !loaded {
		if gs, ok := m.load(ctx, logx.Debugf); ok { // the first update loads them anyway
			m.mu.Lock()
			if m.known == nil {
				m.known = gs
			}
			m.mu.Unlock()
		}
	}
}

// changed notes that group changed, by whom, and compares its members soon,
// as often as the contact list may be loaded; a burst folds into one load.
func (m *members) changed(group string, by feature.Person) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.by == nil {
		m.by = map[string]feature.Person{}
	}
	m.by[group] = by
	if m.busy || m.c.closed.Load() {
		return
	}
	m.busy = true
	gap := m.c.names.minRefresh
	m.c.names.mu.Lock()
	wait := max(0, gap-time.Since(m.c.names.refreshedAt))
	m.c.names.mu.Unlock()
	time.AfterFunc(wait, m.compare)
}

// compare loads the groups' members and hands the core an event for each
// group that changed: who joined and who left, or that the bot was added.
func (m *members) compare() {
	defer m.c.test.Begin()()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	m.c.refresh(ctx) // names for newcomers, if the list may be loaded
	now, ok := m.load(ctx, logx.Warnf)
	m.mu.Lock()
	by, before, handle := m.by, m.known, m.handle
	m.by, m.busy = nil, false
	if ok {
		m.known = now
	}
	m.mu.Unlock()
	if !ok || handle == nil || m.c.closed.Load() {
		return
	}
	for group, who := range by {
		was, seen := before[group]
		is, still := now[group]
		var e feature.Members
		switch {
		case !still || before == nil: // left by the bot, or nothing to compare with
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
		handle(bot.Received{Message: feature.Message{Chat: chat}, Event: &feature.Event{Chat: chat, By: who, Members: &e}})
	}
}

// load is every group the bot is in with its members, by ACI else number;
// a failure is logged with logf.
func (m *members) load(ctx context.Context, logf func(string, ...any)) (map[string][]string, bool) {
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
			if id := cmp.Or(p.UUID, p.Number); id != "" && id != m.c.self() {
				ids = append(ids, id)
			}
		}
		out[g.ID] = ids
	}
	return out, true
}

package signal

import (
	"context"
	"slices"
	"sync"
	"time"
)

// verdictAge is how long whether someone is a group admin is believed, and
// verdictMax how many people's verdicts are held.
const (
	verdictAge = 10 * time.Minute
	verdictMax = 1000
)

// verdicts holds, by sender ID, whether each was lately found an admin of a
// group the bot is a member of, only in memory. A group update forgets them
// all, since an admin may have come or gone; gen counts those, so an answer
// asked for before one isn't kept after it.
type verdicts struct {
	mu  sync.Mutex
	m   map[string]verdict
	gen uint64
}

type verdict struct {
	admin bool
	at    time.Time
}

// get is id's verdict, if fresh.
func (v *verdicts) get(id string) (admin, ok bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	e, ok := v.m[id]
	if !ok || time.Since(e.at) > verdictAge {
		return false, false
	}
	return e.admin, true
}

// generation is how many times the verdicts were forgotten, for put.
func (v *verdicts) generation() uint64 {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.gen
}

// put keeps id's verdict, asked for at generation gen, unless they were
// forgotten since; it makes room by dropping stale ones, or all if none is
// stale.
func (v *verdicts) put(gen uint64, id string, admin bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if gen != v.gen {
		return
	}
	if v.m == nil {
		v.m = map[string]verdict{}
	}
	if _, ok := v.m[id]; !ok && len(v.m) >= verdictMax {
		for k, e := range v.m {
			if time.Since(e.at) > verdictAge {
				delete(v.m, k)
			}
		}
		if len(v.m) >= verdictMax {
			clear(v.m)
		}
	}
	v.m[id] = verdict{admin, time.Now()}
}

// forget drops every verdict.
func (v *verdicts) forget() {
	v.mu.Lock()
	defer v.mu.Unlock()
	clear(v.m)
	v.gen++
}

// dmQuick reports, without calling the network, whether the sender with
// ids (their ACI first, then any number) may DM the bot: listed in
// SIGNAL_DM_USERS, or, if admins may, lately found one. known is false
// when only asking signal-cli can tell. Signal groups have no mods.
func (c *Client) dmQuick(ids ...string) (ok, known bool) {
	if c.dm.Listed(ids...) {
		return true, true
	}
	key := firstID(ids)
	if !c.dm.Roles() || !c.dm.Admins || key == "" {
		return false, true
	}
	return c.verdicts.get(key)
}

// dmAllowed reports whether the sender with ids may DM the bot, asking
// signal-cli, waiting at most 10s, whether they are an admin of a group
// the bot is a member of and answers in (SIGNAL_GROUP_IDS) when that
// decides it, and keeping the answer for verdictAge. A failed lookup
// refuses, with a warning naming no one.
func (c *Client) dmAllowed(ctx context.Context, ids ...string) bool {
	if ok, known := c.dmQuick(ids...); known {
		return ok
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	gen := c.verdicts.generation()
	var gs []group
	if err := c.call(ctx, "listGroups", nil, &gs); err != nil {
		log.Warnf("listGroups failed, so a DM's sender is taken for no admin: %v", err)
		return false
	}
	admin := slices.ContainsFunc(gs, func(g group) bool {
		return g.IsMember && c.groupIDs.has(g.ID) && slices.ContainsFunc(g.Admins, func(m member) bool {
			return slices.ContainsFunc(ids, func(id string) bool { return is(id, m.UUID, m.Number) })
		})
	})
	c.verdicts.put(gen, firstID(ids), admin)
	return admin
}

// firstID is the first of ids not "".
func firstID(ids []string) string {
	if i := slices.IndexFunc(ids, func(id string) bool { return id != "" }); i >= 0 {
		return ids[i]
	}
	return ""
}

// ids is the sender's IDs, an ACI first.
func (e *envelope) ids() []string { return []string{e.SourceUUID, e.Source, e.SourceNumber} }

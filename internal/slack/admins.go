package slack

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/robdevops/signalbot/internal/feature"
)

// verdictAge is how long whether someone is a workspace admin is believed,
// and verdictMax how many people's verdicts are held.
const (
	verdictAge = 10 * time.Minute
	verdictMax = 10000
)

// verdicts holds, by user ID, whether each was lately found an owner or
// admin of the workspace, as users.info last said, only in memory.
type verdicts struct {
	mu sync.Mutex
	m  map[string]verdict
}

type verdict struct {
	admin bool
	at    time.Time
}

// get is user's verdict, if fresh.
func (v *verdicts) get(user string) (admin, ok bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	e, ok := v.m[user]
	if !ok || time.Since(e.at) > verdictAge {
		return false, false
	}
	return e.admin, true
}

// put keeps user's verdict, as users.info answered when asked at asked,
// unless one asked later is kept already, so a slow lookup never replaces
// a newer one's answer; it makes room by dropping stale ones, or all if
// none is stale.
func (v *verdicts) put(user string, admin bool, asked time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.m == nil {
		v.m = map[string]verdict{}
	}
	e, ok := v.m[user]
	if ok && e.at.After(asked) {
		return
	}
	if !ok && len(v.m) >= verdictMax {
		for k, e := range v.m {
			if time.Since(e.at) > verdictAge {
				delete(v.m, k)
			}
		}
		if len(v.m) >= verdictMax {
			clear(v.m)
		}
	}
	v.m[user] = verdict{admin, asked}
}

// isAdmin reports whether user is an owner or admin of the bot's
// workspace: a fresh verdict, else users.info's, once connected, waiting
// at most 10s in all, which is kept for verdictAge. err is a failed lookup.
func (c *Client) isAdmin(ctx context.Context, user string) (bool, error) {
	if admin, ok := c.verdicts.get(user); ok {
		return admin, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if c.WaitID(ctx) == "" { // auth.test says whose workspace it is
		return false, errors.New("not connected after 10s")
	}
	u, err := c.userInfo(ctx, user)
	if err != nil {
		return false, err
	}
	c.learnName(user, u.name)
	return u.admin, nil
}

// dmQuick reports, without calling the network, whether user (a member ID,
// without the prefix) may DM the bot: listed in SLACK_DM_USERS, or, if
// admins may, lately found one. known is false when only users.info can
// tell. Slack has no mods for a bot, so mods alone let no one by role.
func (c *Client) dmQuick(user string) (ok, known bool) {
	if c.dm.Listed(prefix + user) {
		return true, true
	}
	if !c.dm.Roles() || !c.dm.Admins || user == "" {
		return false, true
	}
	return c.verdicts.get(user)
}

// dmAllowed reports whether user may DM the bot, asking users.info when
// that decides it: listed in SLACK_DM_USERS, or, if admins may, a
// workspace owner or admin. With fresh, a verdict kept is not believed,
// for a check that must say whether they may now. A failed lookup
// refuses, with a warning naming no one.
func (c *Client) dmAllowed(ctx context.Context, user string, fresh bool) bool {
	if c.dm.Listed(prefix + user) {
		return true
	}
	if ok, known := c.dmQuick(user); known && !fresh || !c.dm.Roles() || !c.dm.Admins || user == "" {
		return ok
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	u, err := c.userInfo(ctx, user)
	if err != nil {
		log.Warnf("users.info failed, so a DM's user is taken for no admin: %v", err)
		return false
	}
	c.learnName(user, u.name)
	return u.admin
}

// refuse logs a DM from user that SLACK_DM_USERS doesn't allow, by their
// ID alone, once per sender.
func (c *Client) refuse(user string) { c.refused.Log(prefix + user) }

// dmReader reports whether the reader should hand on a DM message or slash
// command from user: not if dmQuick knows they may not, which is logged;
// if it can't tell, the message's Load asks users.info in its slot.
func (c *Client) dmReader(user string) bool {
	if ok, known := c.dmQuick(user); known && !ok {
		c.refuse(user)
		return false
	}
	return true
}

// dmGate is how the reader decides DM traffic it hands on without a slot
// to wait in, a reaction or a press: from user in chat, a DM's allowed
// only if dmQuick knows they may, so an admin whose verdict has gone stale
// is refused until their next DM message asks users.info again.
func (c *Client) dmGate(chat feature.Chat, user string) bool {
	if chat.GroupID != "" {
		return true
	}
	if ok, _ := c.dmQuick(user); ok {
		return true
	}
	c.refuse(user)
	return false
}

package discord

import (
	"context"
	"slices"
	"time"

	"github.com/robdevops/signalbot/internal/kit/logx"
)

// The permissions that make someone a server's admin or mod, as Discord
// numbers its bits. The owner and an administrator have every one.
const (
	permKick           = 1 << 1  // KICK_MEMBERS
	permBan            = 1 << 2  // BAN_MEMBERS
	permManageChannels = 1 << 4  // MANAGE_CHANNELS
	permManageGuild    = 1 << 5  // MANAGE_GUILD
	permManageMessages = 1 << 13 // MANAGE_MESSAGES
	permModerate       = 1 << 40 // MODERATE_MEMBERS

	adminPerms = permAdmin | permManageGuild
	modPerms   = permManageChannels | permManageMessages | permModerate | permKick | permBan
)

// The DM verdicts: how long whether someone is an admin or a mod is
// trusted, and how many are kept.
const (
	verdictTTL  = 10 * time.Minute
	maxVerdicts = 5000
)

// verdict is whether a user is an admin or a mod of any server the bot
// shares with them, as found at a time.
type verdict struct {
	admin, mod bool
	at         time.Time
}

// rank reports whether permissions p make their holder an admin and a
// mod.
func rank(p uint64) (admin, mod bool) {
	return p&adminPerms != 0, p&modPerms != 0
}

// dmDecided reports whether uid may DM the bot, if that can be told
// without a lookup: they are listed (or DISCORD_DM_USER_IDS is "all"), or
// admins and mods may not, or their verdict is still fresh. known is false
// when a lookup is needed.
func (c *Client) dmDecided(uid string) (ok, known bool) {
	if c.dm.Listed(prefix + uid) {
		return true, true
	}
	if !c.dm.Roles() {
		return false, true
	}
	c.mu.Lock()
	v, fresh := c.verdicts[uid]
	c.mu.Unlock()
	if !fresh || time.Since(v.at) >= verdictTTL {
		return false, false
	}
	return c.dm.Allows(func() (bool, bool) { return v.admin, v.mod }), true
}

// mayDM reports whether uid may DM the bot: listed, or, as
// DISCORD_DM_USER_IDS says, an admin or a mod of a server the bot shares
// with them, asked of the API (so not on the reader goroutine) and kept
// 10 minutes. A lookup that fails refuses, with a warning naming no one.
func (c *Client) mayDM(ctx context.Context, uid string) bool {
	if ok, known := c.dmDecided(uid); known {
		return ok
	}
	c.mu.Lock()
	gen := c.forgets
	c.mu.Unlock()
	admin, mod, err := c.serverRoles(ctx, uid)
	ok := c.dm.Allows(func() (bool, bool) { return admin, mod })
	switch {
	case err == nil:
		c.keepVerdict(uid, verdict{admin: admin, mod: mod, at: time.Now()}, gen)
	case !ok:
		logx.Warnf("discord: reading a DM sender's roles failed, so the DM is refused: %v", err)
	}
	return ok
}

// refuseDM logs a DM refused, by its sender's ID only.
func refuseDM(uid string) {
	logx.Infof("%s ignored because not allowed by DISCORD_DM_USER_IDS (add this ID to allow DMs)", prefix+uid)
}

// serverRoles reports whether uid is an admin or a mod of any server the
// bot is in, by their server-wide permissions (@everyone's and their
// roles', without any channel's overwrites). err is any lookup that
// failed, in which case admin and mod are only what the rest said.
func (c *Client) serverRoles(ctx context.Context, uid string) (admin, mod bool, err error) {
	if snowflake(uid) <= 0 {
		return false, false, nil
	}
	c.mu.Lock()
	var guilds []string
	for id := range c.guilds {
		guilds = append(guilds, id)
	}
	c.mu.Unlock()
	slices.Sort(guilds)
	var failed error
	for _, g := range guilds {
		m, err := c.member(ctx, g, uid)
		if err != nil {
			if ctx.Err() != nil {
				return false, false, ctx.Err()
			}
			failed = err
			continue
		}
		if !m.in {
			continue
		}
		c.mu.Lock()
		var p uint64
		if info := c.guilds[g]; info != nil {
			p = c.perms(info, channel{guild: g}, uid, m.roles) // no channel: no overwrites
		}
		c.mu.Unlock()
		a, md := rank(p)
		admin, mod = admin || a, mod || md
		if admin && mod {
			return true, true, nil // no other server can add to it
		}
	}
	return admin, mod, failed
}

// keepVerdict keeps v for uid, within maxVerdicts: those expired go first,
// then any. A verdict looked up from before gen, when the verdicts were
// last forgotten, may already be out of date, so it isn't kept.
func (c *Client) keepVerdict(uid string, v verdict, gen uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen != c.forgets {
		return
	}
	if _, ok := c.verdicts[uid]; !ok && len(c.verdicts) >= maxVerdicts {
		for k, old := range c.verdicts {
			if time.Since(old.at) >= verdictTTL {
				delete(c.verdicts, k)
			}
		}
		for k := range c.verdicts {
			if len(c.verdicts) < maxVerdicts {
				break
			}
			delete(c.verdicts, k)
		}
	}
	c.verdicts[uid] = v
}

// forgetVerdicts drops every DM verdict, under c.mu, when a server's
// owner, roles or the servers the bot is in change: they may no longer
// hold.
func (c *Client) forgetVerdicts() {
	clear(c.verdicts)
	c.forgets++
}

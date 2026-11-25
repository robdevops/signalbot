package telegram

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/robdevops/signalbot/internal/kit/logx"
)

// keepFor is how long a group's administrators, and whether someone may DM
// the bot, are kept before Telegram is asked again.
const keepFor = 10 * time.Minute

// maxVerdicts caps the users whose verdict is kept.
const maxVerdicts = 10000

// adminList is a group's owner and administrators, as getChatAdministrators
// last said, until when.
type adminList struct {
	users []user
	until time.Time
}

// verdict is whether someone may DM the bot, as a lookup last said, until
// when.
type verdict struct {
	ok    bool
	until time.Time
}

// dmKnown is whether user may DM the bot, when that is known without asking
// Telegram: listed in TELEGRAM_DM_USER_IDS (or "all"); refused when no
// role could allow them (no "admins": Telegram has no mods); else a
// verdict from the last 10 minutes.
func (c *Client) dmKnown(user string) (ok, known bool) {
	if c.dm.Listed(user) {
		return true, true
	}
	if !c.dm.Roles() || !c.dm.Admins {
		return false, true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if v, found := c.dmOK[user]; found && time.Now().Before(v.until) {
		return v.ok, true
	}
	return false, false
}

// dmFresh reports whether user may DM the bot, decided without asking
// Telegram, for the reader goroutine: listed, or an admin by a verdict
// from the last 10 minutes. Anyone else is refused.
func (c *Client) dmFresh(user string) bool {
	ok, _ := c.dmKnown(user)
	return ok
}

// dmAllowed reports whether user may DM the bot, asking Telegram whether
// they are an admin if need be, so never on the reader goroutine. The
// verdict is kept 10 minutes. A failed lookup refuses, with a warning
// naming no one, and isn't kept.
func (c *Client) dmAllowed(ctx context.Context, user string) bool {
	if ok, known := c.dmKnown(user); known {
		return ok
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	admin, err := c.isAdmin(ctx, user)
	if err != nil {
		logx.Warnf("telegram: a DM refused: checking whether its sender is an admin failed: %v", err)
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.dmOK) >= maxVerdicts {
		now := time.Now()
		for k, v := range c.dmOK {
			if now.After(v.until) {
				delete(c.dmOK, k)
			}
		}
	}
	if len(c.dmOK) < maxVerdicts {
		c.dmOK[user] = verdict{ok: admin, until: time.Now().Add(keepFor)}
	}
	return admin
}

// refusedDM logs a DM, or a delivered message for one, refused by
// TELEGRAM_DM_USER_IDS, by the sender's ID only.
func refusedDM(user string) {
	logx.Infof("%s ignored because not allowed by TELEGRAM_DM_USER_IDS (add this ID to allow DMs)", user)
}

// isAdmin reports whether user is the owner or an administrator of a group
// the bot has seen since it started (the Bot API can't list a bot's
// groups), asking first of the groups they were seen posting in. A group
// the bot can no longer ask about counts as no; any other failure, unless
// another group says yes, is the error.
func (c *Client) isAdmin(ctx context.Context, user string) (bool, error) {
	c.mu.Lock()
	gs := make([]string, 0, len(c.groups))
	for g := range c.groups {
		gs = append(gs, g)
	}
	posted := func(g string) int {
		if _, ok := c.chats[g][user]; ok {
			return 0
		}
		return 1
	}
	slices.SortFunc(gs, func(a, b string) int { return posted(a) - posted(b) })
	c.mu.Unlock()
	var failed error
	for _, g := range gs {
		as, err := c.groupAdmins(ctx, g)
		switch {
		case gone(err):
		case err != nil:
			if failed = err; ctx.Err() != nil {
				return false, failed
			}
		case slices.ContainsFunc(as, isUser(user)):
			return true, nil
		}
	}
	return false, failed
}

// gone reports whether err is Telegram refusing to say anything of a group:
// it is gone, or the bot is no longer in it.
func gone(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.MigrateTo == 0 && (ae.Code == 400 || ae.Code == 403)
}

// isUser matches the user whose ID is uid.
func isUser(uid string) func(user) bool {
	return func(u user) bool { return id(u.ID) == uid }
}

// isAdminStatus reports whether a member's status is the owner's or an
// administrator's.
func isAdminStatus(status string) bool { return status == "creator" || status == "administrator" }

// groupAdmins is group's owner and administrators, by
// getChatAdministrators, kept 10 minutes; asked at the group's new ID if
// it became a supergroup.
func (c *Client) groupAdmins(ctx context.Context, group string) ([]user, error) {
	c.mu.Lock()
	a, ok := c.admins[group]
	c.mu.Unlock()
	if ok && time.Now().Before(a.until) {
		return a.users, nil
	}
	var got []struct {
		User user `json:"user"`
	}
	params := map[string]string{"chat_id": group}
	err := c.retrying(ctx, group, func(to string) {
		params["chat_id"] = to
		c.migrated(group, to)
	}, func() error { return c.call(ctx, "getChatAdministrators", params, &got) })
	if err != nil {
		return nil, err
	}
	users := make([]user, 0, len(got))
	for _, g := range got {
		users = append(users, g.User)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.admins) >= maxGroups {
		now := time.Now()
		for k, v := range c.admins {
			if now.After(v.until) {
				delete(c.admins, k)
			}
		}
	}
	if _, ok := c.admins[params["chat_id"]]; ok || len(c.admins) < maxGroups {
		c.admins[params["chat_id"]] = adminList{users: users, until: time.Now().Add(keepFor)}
	}
	return users, nil
}

// adminChanged forgets what was kept of a group's administrators and of
// user's verdict, when user became or stopped being one of them.
func (c *Client) adminChanged(u *memberUpd) {
	if isAdminStatus(u.Old.Status) == isAdminStatus(u.New.Status) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.admins, id(u.Chat.ID))
	delete(c.dmOK, id(u.New.User.ID))
}

// forgetNoes forgets the verdicts that refused someone, as a group the bot
// newly knows may make them an admin. c.mu is held.
func (c *Client) forgetNoes() {
	for k, v := range c.dmOK {
		if !v.ok {
			delete(c.dmOK, k)
		}
	}
}

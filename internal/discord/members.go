package discord

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/robdevops/signalbot/internal/kit/logx"
)

// guildMembers is the gateway intent for a guild's members, privileged:
// it lets the bot list them. The bot asks for it, and does without it if
// it is off in the developer portal.
const guildMembers = 1 << 1

// The member lists Delivered reads, with the Server Members intent: at
// most listPages pages of 1,000 members, kept listTTL, for at most
// maxLists guilds at a time.
const (
	listPage  = 1000
	listPages = 5
	listTTL   = 10 * time.Minute
	maxLists  = 20
)

// memberList is a guild's members as last listed, but bots. ready is
// closed once the list is in; ok reports whether it could be read.
type memberList struct {
	ready   chan struct{}
	at      time.Time
	members []listed
	ok      bool
}

// listed is a guild member in its member list: their ID, name and roles.
type listed struct {
	id, name string
	roles    []string
}

// done reports whether l's fetch has ended.
func (l *memberList) done() bool {
	select {
	case <-l.ready:
		return true
	default:
		return false
	}
}

// membersWarning is logged, once, when Discord refuses the Server Members
// intent.
const membersWarning = "discord: Server Members Intent is off, so @mentions in scheduled and delivered posts link only people seen since startup; turn it on in the developer portal, under your application's Bot"

// guildList is guild's members, but bots, read at most once each listTTL
// and one read at a time: a caller meanwhile waits for it, until ctx ends.
// ok is false where the list can't be read: the intent is off, the bot
// may not list them, or the read failed, which is warned of once.
func (c *Client) guildList(ctx context.Context, guild string) ([]listed, bool) {
	if c.membersOff.Load() {
		return nil, false
	}
	c.mu.Lock()
	l := c.lists[guild]
	if l != nil && (!l.done() || time.Since(l.at) < listTTL) {
		c.mu.Unlock()
		select {
		case <-l.ready:
		case <-ctx.Done():
			return nil, false
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		return l.members, l.ok
	}
	if l = c.newList(guild); l == nil {
		c.mu.Unlock()
		return nil, false
	}
	c.mu.Unlock()
	members, err := c.listMembers(ctx, guild)
	c.mu.Lock()
	l.members, l.ok, l.at = members, err == nil, time.Now()
	if err != nil && ctx.Err() != nil { // given up on, not refused: the next caller tries again
		l.at = time.Time{}
	}
	c.mu.Unlock()
	close(l.ready)
	var ae *apiError
	refused := errors.As(err, &ae) && (ae.Status == http.StatusForbidden || ae.Code == 50001)
	if err != nil && ctx.Err() == nil && !refused && c.listWarned.CompareAndSwap(false, true) {
		logx.Warnf("discord: listing a server's members failed, so @mentions link only the people seen since startup: %v", err)
	}
	return members, err == nil
}

// newList starts guild's member list, under c.mu, dropping those expired,
// and the oldest read if there are still maxLists; nil if every one is
// still being read.
func (c *Client) newList(guild string) *memberList {
	var oldest string
	for g, l := range c.lists {
		if !l.done() {
			continue
		}
		if time.Since(l.at) >= listTTL {
			delete(c.lists, g)
		} else if oldest == "" || l.at.Before(c.lists[oldest].at) {
			oldest = g
		}
	}
	if len(c.lists) >= maxLists {
		if oldest == "" {
			return nil
		}
		delete(c.lists, oldest)
	}
	l := &memberList{ready: make(chan struct{})}
	c.lists[guild] = l
	return l
}

// listMembers reads guild's member list (GET /guilds/{guild}/members, with
// the Server Members intent), a page of 1,000 at a time, at most
// listPages, and keeps those that aren't bots and have a name.
func (c *Client) listMembers(ctx context.Context, guild string) ([]listed, error) {
	var out []listed
	after := "0"
	for range listPages {
		var page []member
		if err := c.call(ctx, http.MethodGet, "/guilds/"+guild+"/members?limit="+id(listPage)+"&after="+after, nil, &page); err != nil {
			return nil, err
		}
		for _, m := range page {
			if m.User == nil || snowflake(m.User.ID) <= 0 {
				continue
			}
			if snowflake(m.User.ID) > snowflake(after) {
				after = m.User.ID
			}
			if name := nameOf(m.User, &m); !m.User.Bot && name != "" {
				out = append(out, listed{id: m.User.ID, name: name, roles: m.Roles[:min(len(m.Roles), maxRoles)]})
			}
		}
		if len(page) < listPage {
			break
		}
	}
	return out, nil
}

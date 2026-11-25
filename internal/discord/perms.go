package discord

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// The permissions Groups reads, as Discord numbers its bits.
const (
	permAdmin       = 1 << 3  // ADMINISTRATOR: every permission, in every channel
	permView        = 1 << 10 // VIEW_CHANNEL
	permSend        = 1 << 11 // SEND_MESSAGES
	permSendThreads = 1 << 38 // SEND_MESSAGES_IN_THREADS
)

// The channel types Groups lists to anyone who can view them: a text
// channel, an announcement channel, and their public threads. A private
// thread (12) is listed only to someone seen posting in it.
const (
	typeText          = 0
	typeNews          = 5
	typeNewsThread    = 10
	typePublicThread  = 11
	typePrivateThread = 12
)

// maxRoles caps the roles kept for one guild: Discord's own limit.
const maxRoles = 250

// The member cache: how long a member's roles are trusted, and how many
// are kept.
const (
	memberTTL  = 10 * time.Minute
	maxMembers = 5000
)

// wireChannel is a guild channel or thread as the gateway sends it.
type wireChannel struct {
	ID         string `json:"id"`
	GuildID    string `json:"guild_id"` // not in GUILD_CREATE's lists
	Name       string `json:"name"`
	Type       int    `json:"type"`
	ParentID   string `json:"parent_id"`
	Overwrites []struct {
		ID    string `json:"id"`
		Type  int    `json:"type"` // 0 a role, 1 a member
		Allow string `json:"allow"`
		Deny  string `json:"deny"`
	} `json:"permission_overwrites"`
}

// channel is w as the client keeps it, in guild, called guildName if known.
func (w wireChannel) channel(guild, guildName string) channel {
	ch := channel{guild: guild, name: txt.SafeName(w.Name), guildName: txt.SafeName(guildName), kind: w.Type, parent: w.ParentID}
	for _, o := range w.Overwrites {
		if o.ID != "" && len(ch.overwrites) < 2*maxRoles {
			ch.overwrites = append(ch.overwrites, overwrite{id: o.ID, member: o.Type == 1, allow: permissions(o.Allow), deny: permissions(o.Deny)})
		}
	}
	return ch
}

// permissions is a permission set as Discord writes it, a number in a
// string; 0 if it isn't one.
func permissions(s string) uint64 {
	n, _ := strconv.ParseUint(s, 10, 64)
	return n
}

// setRole keeps r's permissions, within maxRoles.
func (g *guildInfo) setRole(r role) {
	if _, ok := g.roles[r.ID]; r.ID != "" && (ok || len(g.roles) < maxRoles) {
		g.roles[r.ID] = permissions(r.Permissions)
	}
}

// noteGuild remembers a guild's name, owner and roles, under c.mu; roles
// nil leaves those known as they were.
func (c *Client) noteGuild(id, name, owner string, roles []role) {
	for _, r := range roles {
		c.noteRole(id, r)
	}
	g := c.guilds[id]
	if g == nil {
		if len(c.guilds) >= maxGroups {
			return
		}
		g = &guildInfo{roles: map[string]uint64{}}
		c.guilds[id] = g
	}
	g.name, g.owner = cmp.Or(txt.SafeName(name), g.name), cmp.Or(owner, g.owner)
	if roles != nil {
		g.roles = map[string]uint64{}
		for _, r := range roles {
			g.setRole(r)
		}
	}
}

// guildUpdated notes a guild's new name, owner or roles (GUILD_UPDATE).
func (c *Client) guildUpdated(d json.RawMessage) {
	var g struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Owner string `json:"owner_id"`
		Roles []role `json:"roles"`
	}
	if json.Unmarshal(d, &g) != nil || g.ID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.guilds[g.ID] != nil {
		c.noteGuild(g.ID, g.Name, g.Owner, g.Roles)
		c.forgetVerdicts()
	}
}

// memberInfo is a guild member's roles and the end of any timeout, or
// that user isn't a member, as the REST API said at a time.
type memberInfo struct {
	roles []string
	in    bool
	until time.Time // timed out, unable to post, until then
	at    time.Time
}

// member is user in guild: their roles, any timeout, and whether they are
// a member at all, asked of the REST API (GET /guilds/{guild}/members/{user},
// which needs no privileged intent) and kept for 10 minutes.
func (c *Client) member(ctx context.Context, guild, user string) (memberInfo, error) {
	key := guild + "/" + user
	c.mu.Lock()
	m, ok := c.members[key]
	c.mu.Unlock()
	if ok && time.Since(m.at) < memberTTL {
		return m, nil
	}
	var got member
	err := c.call(ctx, http.MethodGet, "/guilds/"+guild+"/members/"+user, nil, &got)
	var ae *apiError
	switch {
	case errors.As(err, &ae) && (ae.Status == http.StatusNotFound || ae.Code == 10007): // an unknown member
		m = memberInfo{at: time.Now()}
	case err != nil:
		return memberInfo{}, err
	default:
		m = memberInfo{roles: got.Roles, in: true, at: time.Now()}
		m.until, _ = time.Parse(time.RFC3339, got.Until)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.members[key]; !ok && len(c.members) >= maxMembers {
		for k, old := range c.members {
			if time.Since(old.at) >= memberTTL {
				delete(c.members, k)
			}
		}
		for k := range c.members { // none expired: any one goes
			if len(c.members) < maxMembers {
				break
			}
			delete(c.members, k)
		}
	}
	c.members[key] = m
	return m, nil
}

// canView reports whether user, with roles, can see ch in g, under c.mu.
func (c *Client) canView(g *guildInfo, ch channel, user string, roles []string) bool {
	return c.perms(g, ch, user, roles)&permView != 0
}

// canPost reports whether member m, user, may post in ch in g, under c.mu:
// see it and send there (in a thread, send in threads), and not be timed
// out, which the owner and administrators never are.
func (c *Client) canPost(g *guildInfo, ch channel, user string, m memberInfo) bool {
	if !m.in {
		return false
	}
	p := c.perms(g, ch, user, m.roles)
	send := uint64(permSend)
	if isThread(ch.kind) {
		send = permSendThreads
	}
	if p&permView == 0 || p&send == 0 {
		return false
	}
	return time.Now().After(m.until) || user == g.owner || p&permAdmin != 0
}

func isThread(kind int) bool {
	return kind == typeNewsThread || kind == typePublicThread || kind == typePrivateThread
}

// perms is user's permissions, with roles, in ch in g, by Discord's own
// reckoning, under c.mu: the @everyone role's permissions and those of
// the user's roles, all of them for the owner or an administrator, then
// the channel's overwrites, @everyone's, the roles' together, and the
// user's own. A thread's are its channel's; one whose channel isn't known
// has none.
func (c *Client) perms(g *guildInfo, ch channel, user string, roles []string) uint64 {
	if isThread(ch.kind) {
		parent, ok := c.channels[ch.parent]
		if !ok {
			return 0
		}
		ch = parent
	}
	perms := g.roles[ch.guild]
	for _, r := range roles {
		perms |= g.roles[r]
	}
	if user == g.owner || perms&permAdmin != 0 {
		return math.MaxUint64
	}
	var allow, deny uint64
	for _, o := range ch.overwrites {
		if !o.member && o.id == ch.guild {
			perms = perms&^o.deny | o.allow
		} else if !o.member && slices.Contains(roles, o.id) {
			allow, deny = allow|o.allow, deny|o.deny
		}
	}
	perms = perms&^deny | allow
	for _, o := range ch.overwrites {
		if o.member && o.id == user {
			perms = perms&^o.deny | o.allow
		}
	}
	return perms
}

// Groups lists, by name, the channels user may post in, in the servers
// they share with the bot: text and announcement channels and their
// public threads, by the server's roles, the channels' permissions and
// any timeout, and a private thread, or a channel the bot knows only from
// a message, where it has also seen them post. Where their roles can't be
// read, it lists only the channels it has seen them post in (Delivered
// checks again before posting); if ctx ends first, it fails. Each is
// marked Admin or Mod by user's permissions in that channel, its
// overwrites counted: Administrator or Manage Server make an admin,
// Manage Channels, Manage Messages, Moderate Members, Kick Members or Ban
// Members a mod, and the owner and an administrator are both.
func (c *Client) Groups(ctx context.Context, user string) ([]feature.Group, error) {
	uid, ok := strings.CutPrefix(user, prefix)
	if !ok || snowflake(uid) <= 0 {
		return nil, nil
	}
	c.mu.Lock()
	var guilds []string
	for id := range c.guilds {
		guilds = append(guilds, id)
	}
	c.mu.Unlock()
	slices.Sort(guilds)
	type standing struct {
		memberInfo
		failed bool
	}
	in := map[string]standing{}
	for _, g := range guilds {
		m, err := c.member(ctx, g, uid)
		if ctx.Err() != nil { // given up on: not a failure for each server
			return nil, ctx.Err()
		}
		if err != nil {
			log.Warnf("reading a server member's roles failed, so only the channels they were seen in are listed: %v", err)
		}
		in[g] = standing{memberInfo: m, failed: err != nil}
	}
	c.mu.Lock()
	var out []feature.Group
	for id, ch := range c.channels {
		seen := c.seenIn[uid][id]
		s, ok := in[ch.guild]
		g := c.guilds[ch.guild]
		var show bool
		switch {
		case !ok || g == nil || s.failed:
			show = seen
		case !s.in:
			show = false
		case ch.kind == typeText, ch.kind == typeNews, ch.kind == typeNewsThread, ch.kind == typePublicThread:
			show = c.canPost(g, ch, uid, s.memberInfo)
		default:
			show = seen && c.canPost(g, ch, uid, s.memberInfo)
		}
		if !show {
			continue
		}
		name := "#" + cmp.Or(ch.name, id)
		if gn := cmp.Or(g.nameOr(), ch.guildName); gn != "" {
			name += " (" + gn + ")"
		}
		grp := feature.Group{ID: prefix + id, Name: name}
		if ok && g != nil && !s.failed && s.in {
			grp.Admin, grp.Mod = rank(c.perms(g, ch, uid, s.roles))
		}
		out = append(out, grp)
	}
	c.mu.Unlock()
	slices.SortFunc(out, func(a, b feature.Group) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID))
	})
	return out, nil
}

// nameOr is g's name, or "" for none.
func (g *guildInfo) nameOr() string {
	if g == nil {
		return ""
	}
	return g.name
}

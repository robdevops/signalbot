package slack

import (
	"cmp"
	"context"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// The limits on what Delivered asks Slack for: how long a list is kept,
// how many members of a channel and names in the workspace are read, how
// many lists are kept at once, and how long one Delivered waits for them.
const (
	rosterTTL   = 10 * time.Minute
	maxMembers  = 5000
	maxNames    = 10000
	maxRoster   = 1000
	rosterWait  = 30 * time.Second
	rosterPage  = "200"
	maxRosterPg = 50
)

// userID is a Slack user ID, as a member list gives it; channelID a
// channel's, public or private, whose members can be listed.
var (
	userID    = regexp.MustCompile(`^[UW][A-Z0-9]{8,20}$`)
	channelID = regexp.MustCompile(`^[CG][A-Z0-9]{8,20}$`)
)

// namesKey is the roster's key for the workspace's names, which no
// channelID matches.
const namesKey = ""

// roster keeps the lists Delivered asks Slack for, each for rosterTTL: by
// the key "" the workspace's names, by a channel ID that channel's
// members. Only one fetch of a key runs at a time; the others wait for it.
type roster struct {
	mu      sync.Mutex
	entries map[string]*rosterEntry
}

// rosterEntry is one list: for the workspace, user ID to name; for a
// channel, its members' IDs, each to "".
type rosterEntry struct {
	busy chan struct{}     // held while the list is fetched
	at   time.Time         // when it was fetched, zero before; under roster.mu
	val  map[string]string // under roster.mu
}

// get is key's list, fetched again by fetch when it is older than
// rosterTTL, waiting no longer than ctx for another fetch of it. A failed
// fetch isn't kept.
func (r *roster) get(ctx context.Context, key string, fetch func(context.Context) (map[string]string, error)) (map[string]string, error) {
	r.mu.Lock()
	if r.entries == nil {
		r.entries = map[string]*rosterEntry{}
	}
	e := r.entries[key]
	if e == nil {
		if len(r.entries) >= maxRoster {
			for k, old := range r.entries {
				// One being fetched (busy) stays, so that a second fetch
				// of its key can't start alongside it.
				if time.Since(old.at) >= rosterTTL && len(old.busy) == 0 {
					delete(r.entries, k)
				}
			}
		}
		if len(r.entries) >= maxRoster {
			r.mu.Unlock()
			return fetch(ctx)
		}
		e = &rosterEntry{busy: make(chan struct{}, 1)}
		r.entries[key] = e
	}
	r.mu.Unlock()
	select {
	case e.busy <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-e.busy }()
	r.mu.Lock()
	val, fresh := e.val, !e.at.IsZero() && time.Since(e.at) < rosterTTL
	r.mu.Unlock()
	if fresh {
		return val, nil
	}
	val, err := fetch(ctx)
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	e.val, e.at = val, time.Now()
	r.mu.Unlock()
	return val, nil
}

// forget drops key's list, so the next get fetches it again.
func (r *roster) forget(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, key)
}

// members are a delivered message's members in channel, sorted by name:
// those Slack lists in it, named as the workspace's list names them, and
// the people seen posting there. If Slack won't list them, such as for an
// app installed without channels:read, groups:read and users:read, they
// are the people seen alone.
func (c *Client) members(ctx context.Context, channel string) []bot.Member {
	names := map[string]string{}
	c.mu.Lock()
	for u := range c.chans[channel] {
		if n := c.people[u]; n != "" {
			names[u] = n
		}
	}
	c.mu.Unlock()
	if !channelID.MatchString(channel) {
		// Nothing Slack could list, nor to be taken for namesKey: the
		// people seen alone.
	} else if listed, err := c.listed(ctx, channel); err != nil {
		if code(err) == "missing_scope" {
			c.membersWarn.Do(func() {
				logx.Warnf("Slack refused to list a channel's members (missing_scope): the app needs the scopes channels:read, groups:read and users:read, so paste the app manifest again at api.slack.com/apps → your app → App Manifest and reinstall the app; until then a delivered message names only the people seen since the bot started")
			})
		} else {
			logx.Warnf("slack: listing a channel's members failed (%s); naming only the people seen", cmp.Or(code(err), "no answer"))
		}
	} else {
		for u, n := range listed {
			names[u] = cmp.Or(names[u], n)
		}
	}
	var out []bot.Member
	for u, n := range names {
		out = append(out, bot.Member{Name: n, ID: prefix + u})
	}
	slices.SortFunc(out, func(a, b bot.Member) int {
		return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID))
	})
	return out
}

// listed are channel's members that the workspace's list names, by ID to
// name.
func (c *Client) listed(ctx context.Context, channel string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, rosterWait)
	defer cancel()
	ids, err := c.roster.get(ctx, channel, func(ctx context.Context) (map[string]string, error) { return c.channelMembers(ctx, channel) })
	if err != nil {
		return nil, err
	}
	names, err := c.roster.get(ctx, namesKey, c.workspaceNames)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for u := range ids {
		if n := names[u]; n != "" {
			out[u] = n
		}
	}
	return out, nil
}

// channelMembers are channel's members' IDs, each to "", by
// conversations.members, page by page, at most maxMembers.
func (c *Client) channelMembers(ctx context.Context, channel string) (map[string]string, error) {
	out := map[string]string{}
	cursor := ""
	for range maxRosterPg {
		vals := url.Values{"channel": {channel}, "limit": {rosterPage}}
		if cursor != "" {
			vals.Set("cursor", cursor)
		}
		var r struct {
			Members []string `json:"members"`
			Meta    struct {
				Next string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := c.form(ctx, "conversations.members", c.token, vals, &r); err != nil {
			return nil, err
		}
		for _, u := range r.Members {
			if userID.MatchString(u) && len(out) < maxMembers {
				out[u] = ""
			}
		}
		if cursor = r.Meta.Next; cursor == "" || len(out) >= maxMembers {
			break
		}
	}
	return out, nil
}

// workspaceNames are the workspace's people, by ID to display name, by
// users.list, page by page, at most maxNames: a display name, else a real
// name, else the user name, made safe. Deleted users and bots, Slackbot
// among them, are left out.
func (c *Client) workspaceNames(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	cursor := ""
	for range maxRosterPg {
		vals := url.Values{"limit": {rosterPage}}
		if cursor != "" {
			vals.Set("cursor", cursor)
		}
		var r struct {
			Members []struct {
				ID       string `json:"id"`
				Name     string `json:"name"`
				RealName string `json:"real_name"`
				Deleted  bool   `json:"deleted"`
				IsBot    bool   `json:"is_bot"`
				Profile  struct {
					DisplayName string `json:"display_name"`
					RealName    string `json:"real_name"`
				} `json:"profile"`
			} `json:"members"`
			Meta struct {
				Next string `json:"next_cursor"`
			} `json:"response_metadata"`
		}
		if err := c.form(ctx, "users.list", c.token, vals, &r); err != nil {
			return nil, err
		}
		for _, u := range r.Members {
			if u.Deleted || u.IsBot || u.ID == "USLACKBOT" || !userID.MatchString(u.ID) || len(out) >= maxNames {
				continue
			}
			if n := txt.SafeName(cmp.Or(u.Profile.DisplayName, u.Profile.RealName, u.RealName, u.Name)); n != "" {
				out[u.ID] = n
			}
		}
		if cursor = r.Meta.Next; cursor == "" || len(out) >= maxNames {
			break
		}
	}
	return out, nil
}

package signal

import (
	"cmp"
	"context"
	"strings"
	"sync"
	"time"

	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// groupSet is which groups the bot answers in (SIGNAL_GROUP_IDS): every
// group, or only those listed, by signal-cli's group ID.
type groupSet struct {
	all bool
	ids map[string]bool
}

// parseGroups reads SIGNAL_GROUP_IDS: group IDs separated by commas,
// semicolons or whitespace, up to any "#", which starts a comment, as in
// the suggested line, whose group names an env file keeps in the value.
// Unset, or "all" anywhere (any case), is every group; "none" adds none,
// so "none" alone is no group at all.
func parseGroups(value string) groupSet {
	value, _, _ = strings.Cut(value, "#") // a group ID is base64, never "#"
	words := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
	if len(words) == 0 {
		return groupSet{all: true}
	}
	s := groupSet{ids: map[string]bool{}}
	for _, w := range words {
		switch strings.ToLower(w) {
		case "all":
			return groupSet{all: true}
		case "none":
		default:
			s.ids[w] = true
		}
	}
	return s
}

// has reports whether the bot answers in group id.
func (s groupSet) has(id string) bool { return s.all || s.ids[id] }

// ignoredMax caps the groups an ignored message is logged for, once each.
const ignoredMax = 1000

// ignored notes the groups a message was ignored in, as not listed, so
// each is logged once a run.
type ignored struct {
	mu   sync.Mutex
	seen map[string]bool
}

// unlisted reports whether group is one the bot doesn't answer in, a DM
// ("") never; the first time each such group is met it is logged, by its
// ID only.
func (c *Client) unlisted(group string) bool {
	if group == "" || c.groupIDs.has(group) {
		return false
	}
	c.ignored.mu.Lock()
	first := !c.ignored.seen[group] && len(c.ignored.seen) < ignoredMax
	if first {
		if c.ignored.seen == nil {
			c.ignored.seen = map[string]bool{}
		}
		c.ignored.seen[group] = true
	}
	c.ignored.mu.Unlock()
	if first {
		logx.Infof("a message in group %s ignored: not in SIGNAL_GROUP_IDS", txt.Clamp(txt.SafeName(group), 64))
	}
	return true
}

// suggestGroups warns, once a run, when the bot answers in any group
// (SIGNAL_GROUP_IDS unset or all: New sets suggest), and suggests the setting that lists the
// groups it is in now, with their names: the one place the bot logs a
// group's name, so whoever runs it can tell which is which.
// A name's backslashes are shown as "/", so the line can be pasted into
// an env file whole.
func (c *Client) suggestGroups(ctx context.Context) {
	if !c.suggest.Swap(false) {
		return
	}
	logx.Warnf("Signal: the bot answers in any group anyone adds it to, as SIGNAL_GROUP_IDS is unset or all; list the groups it should answer in")
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var gs []group
	if err := c.call(ctx, "listGroups", nil, &gs); err != nil {
		logx.Warnf("listGroups failed, so no SIGNAL_GROUP_IDS is suggested: %v", err)
		return
	}
	var ids, names []string
	for _, g := range gs {
		if g.IsMember && g.ID != "" {
			ids = append(ids, g.ID)
			// A backslash shown as "/": in an env file systemd reads, one
			// that ends the line would join the next setting to this one.
			name := strings.ReplaceAll(txt.SafeName(g.Name), `\`, "/")
			names = append(names, cmp.Or(txt.Clamp(name, 40), "(no name)"))
		}
	}
	if len(ids) == 0 {
		logx.Warnf("Signal: the bot is in no group yet; once it is added to yours, list their IDs in SIGNAL_GROUP_IDS (restart the bot to have it suggest the line)")
		return
	}
	logx.Warnf("Signal: to answer only in the groups the bot is in now, set: SIGNAL_GROUP_IDS=%s  # %s", strings.Join(ids, ","), strings.Join(names, ", "))
}

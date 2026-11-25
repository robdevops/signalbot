package signal

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

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

// groupsShown caps the groups the startup line names; the rest are counted.
const groupsShown = 100

// groupName is a group's name as the startup lines show it: made safe and
// cut to 40 characters, its backslashes shown as "/", since in an env file
// systemd reads, one that ends a line would join the next setting to it.
// Control and format characters are dropped: anyone can name a group the
// bot is added to, and one such character would have journalctl show the
// whole line, the warning included, as "[blob data]", or a terminal run
// an escape sequence or reorder the line.
func groupName(name string) string {
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && !unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, name)
	name = strings.ReplaceAll(txt.SafeName(name), `\`, "/")
	return cmp.Or(txt.Clamp(name, 40), "(no name)")
}

// reportGroups lists, once a run (New sets report), the groups the bot is
// a member of, by name and ID, so whoever runs it sees a group it was added
// to by someone else: "Signal groups (N): Name (id), ...", the one place
// the bot logs a group's name. With SIGNAL_GROUP_IDS a list (none
// included), a group it doesn't list is marked as ignored and makes the
// line a warning. With all, the bot warns that it answers in any group,
// and suggests the setting that lists the groups it is in now, with their
// names, a line that can be pasted into an env file whole.
func (c *Client) reportGroups(ctx context.Context) {
	if !c.report.Swap(false) {
		return
	}
	if c.groupIDs.all {
		logx.Warnf("Signal: the bot answers in any group anyone adds it to, as SIGNAL_GROUP_IDS is unset or all; list the groups it should answer in")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var gs []group
	if err := c.call(ctx, "listGroups", nil, &gs); err != nil {
		if c.groupIDs.all {
			logx.Warnf("Signal: listGroups failed, so the groups the bot is in aren't listed, nor SIGNAL_GROUP_IDS suggested: %v", err)
		} else {
			logx.Warnf("Signal: listGroups failed, so the groups the bot is in aren't listed: %v", err)
		}
		return
	}
	var ids, names, listed, unlisted []string
	for _, g := range gs {
		if !g.IsMember || g.ID == "" {
			continue
		}
		name := groupName(g.Name)
		entry := name + " (" + txt.Clamp(txt.SafeName(g.ID), 64) + ")"
		if c.groupIDs.has(g.ID) {
			listed = append(listed, entry)
		} else {
			unlisted = append(unlisted, entry+" — not in SIGNAL_GROUP_IDS, ignored")
		}
		ids, names = append(ids, g.ID), append(names, name)
	}
	// The unlisted first, so a cut never hides one.
	shown := append(unlisted, listed...)
	line := fmt.Sprintf("Signal groups (%d): ", len(shown))
	switch {
	case len(shown) == 0:
		line += "none"
	case len(shown) > groupsShown:
		line += strings.Join(shown[:groupsShown], ", ") + fmt.Sprintf(", and %d more", len(shown)-groupsShown)
	default:
		line += strings.Join(shown, ", ")
	}
	if len(unlisted) > 0 {
		logx.Warnf("%s", line)
	} else {
		logx.Noticef("%s", line)
	}
	if !c.groupIDs.all {
		return
	}
	if len(ids) == 0 {
		logx.Warnf("Signal: the bot is in no group yet; once it is added to yours, list their IDs in SIGNAL_GROUP_IDS (restart the bot to have it suggest the line)")
		return
	}
	logx.Warnf("Signal: to answer only in the groups the bot is in now, set: SIGNAL_GROUP_IDS=%s  # %s", strings.Join(ids, ","), strings.Join(names, ", "))
}

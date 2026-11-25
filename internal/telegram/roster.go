package telegram

import (
	"cmp"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/state"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// The roster is every group the bot knows it is in, saved in
// telegram-groups.json in the state directory, so each startup can tell
// whoever runs the bot where it is: the Bot API can't list a bot's groups.
// A group is added when the bot is added to it, or any update comes from
// it; it is dropped when the bot leaves or is removed, and moved when it
// becomes a supergroup. It is saved at each change, which is rare (a new
// group, a new title, a removal, a move): an update from a group already
// known with the same title writes nothing.

// knownGroup is a group in the roster.
type knownGroup struct {
	Title string `json:"title"` // as Telegram gives it, the newest
	Since string `json:"since"` // the day it was first seen, YYYY-MM-DD
}

// savedGroupID is a saved group's chat ID: a group's is negative.
var savedGroupID = regexp.MustCompile(`^-\d{1,20}$`)

// loadRoster reads the saved roster. A file that can't be read or parsed is
// warned about, without its content, and the bot starts knowing of no
// groups; the next change overwrites it.
func (c *Client) loadRoster() {
	var saved map[string]knownGroup
	found, err := c.rosterDoc.Load(&saved)
	if err != nil {
		logx.Warnf("telegram: %s can't be read, so the bot starts knowing of no groups; it is replaced at the next change", c.rosterDoc.Path())
		return
	}
	if !found {
		return
	}
	for gid, g := range saved {
		if savedGroupID.MatchString(gid) && len(c.roster) < maxGroups {
			c.roster[gid] = g
		}
	}
}

// saveRoster writes the roster. The lock is held from the copy to the
// write, so the last write holds every change made before it.
func (c *Client) saveRoster() {
	c.rosterMu.Lock()
	defer c.rosterMu.Unlock()
	c.mu.Lock()
	snap := maps.Clone(c.roster)
	c.mu.Unlock()
	if err := c.rosterDoc.Save(snap); err != nil {
		logx.Warnf("telegram: saving the groups the bot is in failed: %v", err)
	}
}

// learnGroup notes that the bot is in ch, if it is a group: a group not
// known before is added, with a warning, and a known one's title kept up
// to date.
func (c *Client) learnGroup(ch chat) {
	if ch.Type != "group" && ch.Type != "supergroup" {
		return
	}
	gid := id(ch.ID)
	c.mu.Lock()
	g, ok := c.roster[gid]
	switch {
	case !ok && len(c.roster) >= maxGroups:
		c.mu.Unlock()
		return
	case !ok:
		c.roster[gid] = knownGroup{Title: ch.Title, Since: time.Now().Format(time.DateOnly)}
	case ch.Title != "" && ch.Title != g.Title:
		g.Title = ch.Title
		c.roster[gid] = g
	default:
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	if !ok {
		logx.Warnf("Telegram: the bot is in a new group: %s", listed(gid, ch.Title))
	}
	c.saveRoster()
}

// dropGroup takes group out of the roster: the bot left it or was removed.
func (c *Client) dropGroup(group string) {
	c.mu.Lock()
	_, ok := c.roster[group]
	delete(c.roster, group)
	c.mu.Unlock()
	if ok {
		c.saveRoster()
	}
}

// moveRoster moves a group's entry to its new ID, after it became a
// supergroup, and reports whether anything changed. c.mu is held.
func (c *Client) moveRoster(from, to string) bool {
	g, ok := c.roster[from]
	if !ok {
		return false
	}
	delete(c.roster, from)
	if _, there := c.roster[to]; !there {
		c.roster[to] = g
	}
	return true
}

// logRoster logs the groups the roster holds, at startup, so whoever runs
// the bot sees where it is, and can spot a group they didn't add it to.
func (c *Client) logRoster() {
	c.mu.Lock()
	type entry struct{ id, title string }
	var gs []entry
	for gid, g := range c.roster {
		gs = append(gs, entry{gid, g.Title})
	}
	c.mu.Unlock()
	if len(gs) == 0 {
		logx.Noticef("Telegram groups the bot knows of: none")
		return
	}
	slices.SortFunc(gs, func(a, b entry) int {
		return cmp.Or(strings.Compare(strings.ToLower(a.title), strings.ToLower(b.title)), strings.Compare(a.id, b.id))
	})
	var parts []string
	for _, g := range gs {
		parts = append(parts, listed(g.id, g.title))
	}
	logx.Noticef("Telegram groups the bot knows of (%d): %s", len(gs), strings.Join(parts, ", "))
}

// listed is a group as the log shows it: "Title (id)", its title made safe
// and cut to 40 characters, a backslash shown as "/" (in an env file
// systemd reads, one that ends a line would join the next to it). These
// lines are the only ones that hold a group's title.
func listed(gid, title string) string {
	t := txt.Clamp(strings.ReplaceAll(txt.SafeName(title), `\`, "/"), 40)
	return fmt.Sprintf("%s (%s)", cmp.Or(t, "(no title)"), gid)
}

// rosterDoc is the roster's file in dir.
func rosterDoc(dir string) *state.Doc { return state.Open(state.Dir(dir), state.TelegramGroups) }

package discord

import (
	"context"
	"time"

	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// keepDays is how long a person's name is kept in a channel where they
// haven't been seen since.
const keepDays = 180

// person is someone seen in a channel: their name there, and the day, in
// UTC, they were last seen there.
type person struct {
	name, day string
}

// today is the day, in UTC, as a person's day is written.
func today() string { return time.Now().UTC().Format(time.DateOnly) }

// cutoff is the first day whose people are still kept.
func cutoff() string { return time.Now().UTC().AddDate(0, 0, -keepDays).Format(time.DateOnly) }

// savedNames is discord-people.json: each channel's people, by channel
// and user ID.
type savedNames struct {
	Channels map[string]map[string]savedPerson `json:"channels"`
}

type savedPerson struct {
	Name string `json:"name"`
	Seen string `json:"seen"` // the day last seen there, 2006-01-02 in UTC
}

// loadNames reads the people saved in each channel, but those not seen
// for keepDays, so a restart still knows the names Delivered's members
// and Groups use. A file that can't be read is logged, without its
// contents, and replaced at the next save.
func (c *Client) loadNames() {
	var f savedNames
	if _, err := c.names.Load(&f); err != nil {
		logx.Warnf("discord: %s can't be read, so the names saved in it are forgotten", c.names.Path())
		return
	}
	old, now := cutoff(), today()
	c.mu.Lock()
	defer c.mu.Unlock()
	dropped := false
	latest := map[string]person{} // each person's name where last seen
	for ch, people := range f.Channels {
		for uid, p := range people {
			_, err := time.Parse(time.DateOnly, p.Seen)
			name := txt.SafeName(p.Name)
			if snowflake(ch) <= 0 || snowflake(uid) <= 0 || name == "" || err != nil || p.Seen < old {
				dropped = true
				continue
			}
			if p.Seen > now { // a day to come would never expire
				p.Seen, dropped = now, true
			}
			c.note(ch, uid, person{name: name, day: p.Seen})
			if l, ok := latest[uid]; !ok || p.Seen > l.day || p.Seen == l.day && name < l.name {
				latest[uid] = person{name: name, day: p.Seen}
			}
		}
	}
	for uid, p := range latest {
		if _, ok := c.people[uid]; ok {
			c.people[uid] = p.name
		}
	}
	c.dirty, c.pruned = dropped, now
}

// prune forgets, under c.mu, each person in a channel they haven't been
// seen in for keepDays, and anyone seen in no channel since.
func (c *Client) prune() {
	old := cutoff()
	gone := map[string]bool{}
	for ch, people := range c.chats {
		for uid, p := range people {
			if p.day >= old {
				continue
			}
			delete(people, uid)
			delete(c.seenIn[uid], ch)
			gone[uid], c.dirty = true, true
		}
		if len(people) == 0 {
			delete(c.chats, ch)
		}
	}
	for _, people := range c.chats { // still seen somewhere
		if len(gone) == 0 {
			break
		}
		for uid := range people {
			delete(gone, uid)
		}
	}
	for uid := range gone {
		delete(c.seenIn, uid)
		delete(c.people, uid)
	}
}

// saveNames saves each channel's people, if they changed, once a day
// forgetting those not seen for keepDays first. A failure is logged.
func (c *Client) saveNames() {
	c.mu.Lock()
	if d := today(); d != c.pruned {
		c.prune()
		c.pruned = d
	}
	if !c.dirty {
		c.mu.Unlock()
		return
	}
	f := savedNames{Channels: map[string]map[string]savedPerson{}}
	for ch, people := range c.chats {
		saved := map[string]savedPerson{}
		for uid, p := range people {
			saved[uid] = savedPerson{Name: p.name, Seen: p.day}
		}
		f.Channels[ch] = saved
	}
	c.dirty = false // a failure isn't retried every minute, only after the next change
	c.mu.Unlock()
	if err := c.names.Save(f); err != nil {
		logx.Warnf("discord: saving the names seen failed: %v", err)
	}
}

// keepNames saves the names seen at most every saveEvery while they
// change, and once more when ctx ends.
func (c *Client) keepNames(ctx context.Context) {
	t := time.NewTicker(c.saveEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			c.saveNames()
			return
		case <-t.C:
			c.saveNames()
		}
	}
}

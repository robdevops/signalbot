package slack

import (
	"context"
	"time"

	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// PeopleFile is the people seen in each channel, saved in the state
// directory so their names survive a restart.
const PeopleFile = peopleName + ".json"

const peopleName = "slack-people"

// The caps on what the client remembers: channels, people in one, and
// names in all.
const (
	maxChans      = 1000
	maxChanPeople = 1000
	maxPeople     = 10000
)

// forget is how long someone not seen in a channel is remembered there.
const forget = 180 * 24 * time.Hour

// seenPerson is one person in a channel, as saved.
type seenPerson struct {
	Name string `json:"name,omitempty"`
	Seen string `json:"seen"` // the day last seen there, 2006-01-02
}

// savedPeople is slack-people.json: channel ID to user ID to the person.
type savedPeople struct {
	Channels map[string]map[string]seenPerson `json:"channels"`
}

// stale reports whether day, 2006-01-02, is too long ago to keep, or
// can't be read.
func stale(day string, now time.Time) bool {
	t, err := time.Parse(time.DateOnly, day)
	return err != nil || now.Sub(t) > forget
}

// loadPeople reads the people saved before a restart into chans and
// people, but those not seen for 180 days. A file that can't be read is
// logged, without its content, and ignored.
func (c *Client) loadPeople() {
	var s savedPeople
	if _, err := c.saved.Load(&s); err != nil {
		logx.Warnf("slack: %s can't be read; starting without the names saved there", PeopleFile)
		return
	}
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for ch, people := range s.Channels {
		for user, p := range people {
			if ch == "" || user == "" || stale(p.Seen, now) {
				continue
			}
			in := c.chans[ch]
			if in == nil {
				if len(c.chans) >= maxChans {
					break
				}
				in = map[string]string{}
				c.chans[ch] = in
			}
			if len(in) < maxChanPeople {
				in[user] = p.Seen
			}
			if n := txt.SafeName(p.Name); n != "" && len(c.people) < maxPeople {
				c.people[user] = n
			}
		}
	}
}

// keepPeople saves the people seen at most every saveEvery while they
// change, until ctx ends.
func (c *Client) keepPeople(ctx context.Context) {
	tick := time.NewTicker(c.saveEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			c.savePeople()
		}
	}
}

// savePeople saves the people seen in each channel, with their names, if
// they changed since last saved, first forgetting those not seen for 180
// days and the channels the bot left.
func (c *Client) savePeople() {
	if !c.dirty.Swap(false) {
		return
	}
	now := time.Now()
	s := savedPeople{Channels: map[string]map[string]seenPerson{}}
	c.mu.Lock()
	for ch, people := range c.chans {
		if c.left[ch] {
			delete(c.chans, ch)
			continue
		}
		out := map[string]seenPerson{}
		for user, day := range people {
			if stale(day, now) {
				delete(people, user)
				continue
			}
			out[user] = seenPerson{Name: c.people[user], Seen: day}
		}
		if len(out) == 0 {
			delete(c.chans, ch)
			continue
		}
		s.Channels[ch] = out
	}
	c.mu.Unlock()
	if err := c.saved.Save(s); err != nil {
		c.dirty.Store(true)
		logx.Warnf("slack: saving %s failed: %v", PeopleFile, err)
	}
}

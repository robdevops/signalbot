package bot

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/state"
)

// budget caps what each chat, a group or a DM, may spend on the model a
// day (DAILY_BUDGET), midnight to midnight local time. Today's spend is
// saved in budget.json, under a hash of each chat's key, so a restart
// doesn't reset it and the file holds no one's ID.
type budget struct {
	mu    sync.Mutex
	doc   *state.Doc
	limit float64
	now   func() time.Time
	day   string            // today, "2006-01-02", local time
	chats map[string]*spend // by hashed chat key
}

type spend struct {
	Spent float64 `json:"spent"` // US dollars
	Told  bool    `json:"told"`  // the chat was told it reached the limit
}

type budgetFile struct {
	Day   string            `json:"day"`
	Chats map[string]*spend `json:"chats"`
}

// budgetName is the budget's saved state, budget.json, which no feature
// may open.
const budgetName = state.Reserved

// newBudget loads today's spend from dir's budget.json, if it has one for
// today; a file it can't read starts the day afresh, with a warning.
func newBudget(dir string, limit float64, now func() time.Time) *budget {
	b := &budget{doc: state.Open(state.Dir(dir), budgetName), limit: limit, now: now, chats: map[string]*spend{}}
	b.day = b.today()
	var f budgetFile
	switch found, err := b.doc.Load(&f); {
	case err != nil:
		logx.Warnf("%v, so today's spending starts at 0", err)
	case found && f.Day == b.day && f.Chats != nil:
		b.chats = f.Chats
	}
	return b
}

func (b *budget) today() string { return b.now().In(time.Local).Format(time.DateOnly) }

// roll starts a new day's spending once the date changes, saving it at
// once so yesterday's spending doesn't stay on disk. b.mu is held.
func (b *budget) roll() {
	if d := b.today(); d != b.day {
		had := len(b.chats) > 0
		b.day, b.chats = d, map[string]*spend{}
		if had {
			b.save()
		}
	}
}

// of is the chat's spending today; with create, made if it has none, else
// nil.
func (b *budget) of(key string, create bool) *spend {
	sum := sha256.Sum256([]byte(key))
	k := hex.EncodeToString(sum[:])
	if b.chats[k] == nil && create {
		b.chats[k] = &spend{}
	}
	return b.chats[k]
}

// over reports whether the chat has spent its limit today, and if so,
// whether it is the first time it is asked today (tell), and the time
// left until midnight.
func (b *budget) over(key string) (over, tell bool, left time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.roll()
	s := b.of(key, false)
	if s == nil || s.Spent < b.limit {
		return false, false, 0
	}
	if tell = !s.Told; tell {
		s.Told = true
		b.save()
	}
	now := b.now().In(time.Local)
	y, m, d := now.Date()
	return true, tell, time.Date(y, m, d+1, 0, 0, 0, 0, time.Local).Sub(now)
}

// add records cost against the chat's spend today.
func (b *budget) add(key string, cost float64) {
	if cost <= 0 || math.IsNaN(cost) {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.roll()
	b.of(key, true).Spent += cost
	b.save()
}

// save writes today's spending. b.mu is held.
func (b *budget) save() {
	if err := b.doc.Save(budgetFile{Day: b.day, Chats: b.chats}); err != nil {
		logx.Warnf("could not save today's spending: %v", err)
	}
}

// limitNotice tells a chat it has reached today's limit and when the bot
// will answer again: left rounded up to whole minutes, and from 60 minutes
// to whole hours.
func limitNotice(left time.Duration) string {
	when := plural(int(math.Ceil(left.Minutes())), "minute")
	if left > 59*time.Minute {
		when = plural(int(math.Ceil(left.Hours())), "hour")
	}
	return "I've reached today's spending limit for this chat; I'll be back in " + when + "."
}

func plural(n int, unit string) string {
	n = max(n, 1)
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// Package history is the bot's short memory of each chat: a few recent
// turns, each forgotten after a day at most, or sooner when the chat's
// disappearing-messages timer runs out. Messages are only ever in memory.
package history

import (
	"slices"
	"sync"
	"time"

	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/kit/bound"
)

// MaxAge is the longest any message is kept. A chat's shorter
// disappearing-messages timer wins.
const MaxAge = 24 * time.Hour

// maxChats bounds the chats remembered: chat ids come from the network.
const maxChats = 500

// Entry is one remembered message.
type Entry struct {
	Role    string // "user" or "assistant"
	Content string
	Expires time.Time // zero: kept until pushed out by newer turns
}

// Expiry is when a message handled at now must be forgotten.
func Expiry(now time.Time, disappearing time.Duration) time.Time {
	if disappearing <= 0 {
		disappearing = MaxAge
	}
	return now.Add(min(disappearing, MaxAge))
}

// eraser is a chat's pending timer and when it fires.
type eraser struct {
	timer *time.Timer
	at    time.Time
}

// Store holds the history of every chat.
type Store struct {
	turns func() int // turns kept per chat; a turn is two entries

	mu       sync.Mutex
	chats    map[chat.ChatID][]Entry
	erasers  map[chat.ChatID]eraser // one timer per chat, for its next entry to expire
	onForget []func(chat.ChatID)
}

// New keeps turns() turns per chat.
func New(turns func() int) *Store {
	return &Store{turns: turns, chats: map[chat.ChatID][]Entry{}, erasers: map[chat.ChatID]eraser{}}
}

// OnForget registers fn to run when a chat's history is erased entirely, so
// state kept about the chat elsewhere goes with it.
func (s *Store) OnForget(fn func(chat.ChatID)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onForget = append(s.onForget, fn)
}

// Get returns the chat's unexpired entries.
func (s *Store) Get(c chat.ChatID) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return unexpired(s.chats[c], time.Now())
}

// Add appends entries to the chat, keeping its last turns.
func (s *Store) Add(c chat.ChatID, entries ...Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bound.MakeRoom(s.chats, c, maxChats)
	h := append(unexpired(s.chats[c], time.Now()), entries...)
	if max := s.turns() * 2; len(h) > max {
		h = append([]Entry(nil), h[len(h)-max:]...)
	}
	s.chats[c] = h
	// Erase the expired entries and let the garbage collector reclaim them.
	s.scheduleErase(c)
}

// Len is the number of chats with history.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.chats)
}

// unexpired returns a copy of h without the entries whose time has passed.
func unexpired(h []Entry, now time.Time) []Entry {
	return slices.DeleteFunc(slices.Clone(h), func(e Entry) bool {
		return !e.Expires.IsZero() && !now.Before(e.Expires)
	})
}

// forget erases the chat's expired entries from memory, sets the timer for
// the next one, and tells OnForget when nothing is left.
func (s *Store) forget(c chat.ChatID) {
	s.mu.Lock()
	delete(s.erasers, c)
	var gone []func(chat.ChatID)
	if h := unexpired(s.chats[c], time.Now()); len(h) > 0 {
		s.chats[c] = h
	} else {
		delete(s.chats, c)
		gone = slices.Clone(s.onForget)
	}
	s.scheduleErase(c)
	s.mu.Unlock()
	for _, fn := range gone {
		fn(c)
	}
}

// scheduleErase makes sure a timer will fire when the chat's earliest entry
// expires. One timer per chat, however many entries, so a busy group can't
// pile up timers. Caller holds s.mu.
func (s *Store) scheduleErase(c chat.ChatID) {
	var next time.Time
	for _, e := range s.chats[c] {
		if !e.Expires.IsZero() && (next.IsZero() || e.Expires.Before(next)) {
			next = e.Expires
		}
	}
	e, ok := s.erasers[c]
	switch {
	case next.IsZero():
		if ok {
			e.timer.Stop()
			delete(s.erasers, c)
		}
	case !ok || next.Before(e.at):
		if ok {
			e.timer.Stop()
		}
		s.erasers[c] = eraser{time.AfterFunc(time.Until(next), func() { s.forget(c) }), next}
	}
}

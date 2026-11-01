// Package chats keeps the per-chat state: the history and its expiry, recent
// message keys, the names to link @mentions to, the tool groups' follow-up
// counts, and the mailbox. One lock guards it all and is held only to read
// or change it.
package chats

import (
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/robdevops/signalbot/internal/feature"
)

const (
	// MaxChats caps the chats kept: chat keys come from the network.
	MaxChats = 500
	// MaxAge is the longest anything is kept in history. A chat's shorter
	// disappearing-messages timer wins.
	MaxAge = 24 * time.Hour
	// Window is how long a message key is remembered, to drop a repeat.
	Window = 10 * time.Second

	maxQueue  = 4   // messages waiting in one chat's mailbox
	maxNames  = 256 // names per chat
	maxRecent = 64  // message keys per chat
)

var (
	errFull      = errors.New("too many chats")
	errQueueFull = errors.New("the chat's mailbox is full")
	ErrNoSlot    = errors.New("no worker slot is free")
)

// Entry is one message in a chat's history.
type Entry struct {
	Role    string    `json:"role"` // "user" or "assistant"
	Content string    `json:"content"`
	Expires time.Time `json:"-"`
}

// Store is every chat's state.
type Store struct {
	mu     sync.Mutex
	chats  map[string]*chat
	turns  int                      // exchanges kept per chat
	max    int                      // chats kept
	handle func([]feature.Received) // a chat's worker hands it what waits
	spawn  func(worker func()) bool // starts a chat's worker on a free slot
}

type chat struct {
	history   []Entry
	timer     *time.Timer          // fires when the earliest entry or message key lapses
	recent    map[string]time.Time // message key -> when seen
	names     map[string]string    // lowercased name -> ID; "" if ambiguous
	followUps map[int]int          // tool group -> replies left
	queue     []feature.Received   // waiting for the worker
	busy      bool                 // a worker is running
}

// New keeps up to turns exchanges (2 entries each) per chat. A chat with
// messages has one worker, which hands handle everything waiting in the
// mailbox at once, in order, until nothing is left, and then exits. spawn
// must start the worker on a free slot without blocking and return true, or
// return false when no slot is free.
func New(turns int, handle func([]feature.Received), spawn func(worker func()) bool) *Store {
	return &Store{chats: map[string]*chat{}, turns: turns, max: MaxChats, handle: handle, spawn: spawn}
}

// get returns the chat, creating it if create is set. When the map is full,
// an idle chat is dropped to make room; with none idle it returns nil.
// Caller holds s.mu.
func (s *Store) get(key string, create bool) *chat {
	if c := s.chats[key]; c != nil || !create {
		return c
	}
	if len(s.chats) >= s.max {
		evicted := false
		for k, c := range s.chats {
			if !c.busy {
				s.drop(k, c)
				evicted = true
				break
			}
		}
		if !evicted {
			return nil
		}
	}
	c := &chat{}
	s.chats[key] = c
	return c
}

// drop deletes a chat. Caller holds s.mu, and the chat is idle.
func (s *Store) drop(key string, c *chat) {
	if c.timer != nil {
		c.timer.Stop()
	}
	delete(s.chats, key)
}

// tidy forgets the chat's expired entries. When none are left, everything
// kept about the chat goes too, and the chat itself once it is idle and its
// message keys have lapsed. It sets the chat's timer for the next time
// something lapses: one timer per chat. Caller holds s.mu.
func (s *Store) tidy(key string, c *chat) {
	now := time.Now()
	c.history = unexpired(c.history, now)
	var next time.Time
	soonest := func(t time.Time) {
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}
	for _, e := range c.history {
		soonest(e.Expires)
	}
	if len(c.history) == 0 {
		c.history, c.names, c.followUps = nil, nil, nil
		for k, t := range c.recent {
			if now.Sub(t) >= Window {
				delete(c.recent, k)
			} else {
				soonest(t.Add(Window))
			}
		}
		if !c.busy && len(c.recent) == 0 {
			s.drop(key, c)
			return
		}
	}
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	if !next.IsZero() {
		c.timer = time.AfterFunc(time.Until(next), func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.chats[key] == c { // not dropped meanwhile
				s.tidy(key, c)
			}
		})
	}
}

// Post adds r to its chat's mailbox, starting the chat's worker if it has
// none. It returns ErrNoSlot when no slot is free for the worker.
func (s *Store) Post(r feature.Received) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := r.Chat.Key()
	c := s.get(key, true)
	switch {
	case c == nil:
		return errFull
	case c.busy && len(c.queue) >= maxQueue:
		return errQueueFull
	case c.busy:
		c.queue = append(c.queue, r)
		return nil
	}
	c.busy = true
	if !s.spawn(func() { s.work(key, c, []feature.Received{r}) }) {
		c.busy = false
		s.tidy(key, c)
		return ErrNoSlot
	}
	return nil
}

func (s *Store) work(key string, c *chat, rs []feature.Received) {
	for len(rs) > 0 {
		s.handle(rs)
		s.mu.Lock()
		rs, c.queue = c.queue, nil
		if len(rs) == 0 {
			c.busy = false
			s.tidy(key, c)
		}
		s.mu.Unlock()
	}
}

// Repeat reports whether the chat saw message key id within Window, and
// records it.
func (s *Store) Repeat(key, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.get(key, true)
	if c == nil {
		return false
	}
	now := time.Now()
	for k, t := range c.recent {
		if now.Sub(t) >= Window {
			delete(c.recent, k)
		}
	}
	if _, ok := c.recent[id]; ok {
		return true
	}
	if c.recent == nil {
		c.recent = map[string]time.Time{}
	}
	if len(c.recent) < maxRecent {
		c.recent[id] = now
	}
	return false
}

// History is a copy of the chat's unexpired history.
func (s *Store) History(key string) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.get(key, false); c != nil {
		return unexpired(c.history, time.Now())
	}
	return nil
}

// Add appends entries to the chat's history, to be forgotten MaxAge after
// start (when the message arrived), or disappearing after it if that is
// shorter and not 0. Only the latest exchanges are kept.
func (s *Store) Add(key string, start time.Time, disappearing time.Duration, entries ...Entry) {
	if disappearing <= 0 {
		disappearing = MaxAge
	}
	expires := start.Add(min(disappearing, MaxAge))
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.get(key, true)
	if c == nil {
		return
	}
	h := unexpired(c.history, time.Now())
	for _, e := range entries {
		e.Expires = expires
		h = append(h, e)
	}
	if n := 2 * s.turns; len(h) > n {
		h = append([]Entry(nil), h[len(h)-n:]...)
	}
	c.history = h
	s.tidy(key, c)
}

func unexpired(h []Entry, now time.Time) []Entry {
	var out []Entry
	for _, e := range h {
		if now.Before(e.Expires) {
			out = append(out, e)
		}
	}
	return out
}

// Learn records that @name in the chat means the user id.
func (s *Store) Learn(key, name, id string) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.get(key, true); c != nil {
		if c.names == nil {
			c.names = map[string]string{}
		}
		Claim(c.names, name, id)
	}
}

// Claim records in names (lowercased name to ID) that name means id; an id
// of "" can never be linked. Display names are chosen by their owners, so
// anyone can claim a member's name: a name claimed by two people is
// ambiguous, and its ID becomes "". Names match in any case.
func Claim(names map[string]string, name, id string) {
	name = strings.ToLower(name)
	old, ok := names[name]
	switch {
	case name == "":
	case !ok && len(names) < maxNames:
		names[name] = id
	case ok && old != id:
		names[name] = ""
	}
}

// Names is a copy of the chat's names: lowercased name -> ID, "" if ambiguous.
func (s *Store) Names(key string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	if c := s.get(key, false); c != nil {
		for n, id := range c.names {
			out[n] = id
		}
	}
	return out
}

// ForgetNames drops the chat's names, as when a group's members change.
func (s *Store) ForgetNames(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.get(key, false); c != nil {
		c.names = nil
	}
}

// FollowUp is the replies left that offer tool group g in the chat.
func (s *Store) FollowUp(key string, g int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.get(key, false); c != nil {
		return c.followUps[g]
	}
	return 0
}

// SetFollowUp sets the replies left that offer tool group g in the chat.
func (s *Store) SetFollowUp(key string, g, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.get(key, n > 0)
	switch {
	case c == nil:
	case n <= 0:
		delete(c.followUps, g)
	default:
		if c.followUps == nil {
			c.followUps = map[int]int{}
		}
		c.followUps[g] = n
	}
}

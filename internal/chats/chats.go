// Package chats keeps the per-chat state: the history and its expiry, recent
// message keys, the names to link @mentions to, the tool groups' follow-up
// counts, and the mailbox. One lock guards it all and is held only to read
// or change it.
package chats

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	// maxChats caps the chats kept: chat keys come from the network.
	maxChats = 500
	// maxAge is the longest anything is kept in history. A chat's shorter
	// disappearing-messages timer wins.
	maxAge = 24 * time.Hour
	// window is how long a message key is remembered, to drop a repeat.
	window = 10 * time.Second

	maxQueue  = 4   // messages waiting in one chat's mailbox
	maxNames  = 256 // names per chat
	maxRecent = 64  // message keys per chat
	maxGone   = 64  // deleted or edited messages remembered per chat, for ones still waiting
)

var (
	errFull      = fmt.Errorf("%d chats are already held in memory, none idle", maxChats)
	errQueueFull = fmt.Errorf("%d messages are already waiting in this chat", maxQueue)
	ErrNoSlot    = errors.New("no worker slot is free")
)

// Entry is one message in a chat's history.
type Entry struct {
	Role    string // "user" or "assistant"
	Content string
	Context bool  // a message not addressed to the bot, kept only as context
	Refs    []Ref // the messages it holds, so one deleted or edited can be forgotten
	Expires time.Time
}

// Ref names a message in history: ID by its sender and when it was first
// sent, At the version held (when it was sent, or for an edit when it was
// edited), so an edit forgets only the versions before it.
type Ref struct {
	ID string
	At int64
}

// Store is every chat's state, with mailboxes of M.
type Store[M any] struct {
	mu       sync.Mutex
	chats    map[string]*chat[M]
	messages int                      // messages kept per chat
	max      int                      // chats kept
	handle   func([]M)                // a chat's worker hands it what waits
	spawn    func(worker func()) bool // starts a chat's worker on a free slot
	now      func() time.Time         // the clock entries expire by
}

type chat[M any] struct {
	history   []Entry
	timer     *time.Timer          // fires when the earliest entry or message key lapses
	recent    map[string]time.Time // message key -> when seen
	names     map[string]string    // lowercased name -> ID; "" if ambiguous
	followUps map[int]int          // tool group -> replies left
	gone      map[string]int64     // message ID -> versions before this are deleted or replaced
	queue     []M                  // waiting for the worker
	busy      bool                 // a worker is running
}

// keep is how many of the latest entries a history over its cap of n keeps.
// It drops to half of n (rounded up), not by one, so the start of the
// history, and so of the model's request, stays the same for several
// messages and is read from the model API's prompt cache, rather than
// changing with every message once the history is full. Under 3 it keeps n:
// half of 2 would leave one message.
func keep(n int) int {
	if n < 3 {
		return n
	}
	return n - n/2
}

// New keeps up to messages entries per chat, at least half of them once
// that many have been kept (see keep). A chat with
// messages has one worker, which hands handle everything waiting in the
// mailbox at once, in order, until nothing is left, and then exits. spawn
// must start the worker on a free slot without blocking and return true, or
// return false when no slot is free.
func New[M any](messages int, handle func([]M), spawn func(worker func()) bool) *Store[M] {
	return &Store[M]{chats: map[string]*chat[M]{}, messages: messages, max: maxChats, handle: handle, spawn: spawn, now: time.Now}
}

// SetClock makes entries and message keys lapse by now's time instead of
// the real one, for tests that can't wait. Call it before the store is used.
func (s *Store[M]) SetClock(now func() time.Time) { s.now = now }

// get returns the chat, creating it if create is set. When the map is full,
// an idle chat is dropped to make room; with none idle it returns nil.
// Caller holds s.mu.
func (s *Store[M]) get(key string, create bool) *chat[M] {
	if c := s.chats[key]; c != nil || !create {
		return c
	}
	for k, c := range s.chats {
		if len(s.chats) < s.max {
			break
		}
		if !c.busy {
			s.drop(k, c)
		}
	}
	if len(s.chats) >= s.max {
		return nil
	}
	c := &chat[M]{}
	s.chats[key] = c
	return c
}

// drop deletes a chat. Caller holds s.mu, and the chat is idle.
func (s *Store[M]) drop(key string, c *chat[M]) {
	if c.timer != nil {
		c.timer.Stop()
	}
	delete(s.chats, key)
}

// tidy forgets the chat's expired entries. When none are left, everything
// kept about the chat goes too, and the chat itself once it is idle and its
// message keys have lapsed. It sets the chat's timer for the next time
// something lapses: one timer per chat. Caller holds s.mu.
func (s *Store[M]) tidy(key string, c *chat[M]) {
	now := s.now()
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
		if !c.busy && len(c.queue) == 0 {
			c.gone = nil // nothing waits that a delete or edit could still catch
		}
		for k, t := range c.recent {
			if now.Sub(t) >= window {
				delete(c.recent, k)
			} else {
				soonest(t.Add(window))
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
		c.timer = time.AfterFunc(next.Sub(now), func() {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.chats[key] == c { // not dropped meanwhile
				s.tidy(key, c)
			}
		})
	}
}

// Post adds m to the chat's mailbox, starting the chat's worker if it has
// none. It returns ErrNoSlot when no slot is free for the worker.
func (s *Store[M]) Post(key string, m M) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.get(key, true)
	switch {
	case c == nil:
		return errFull
	case c.busy && len(c.queue) >= maxQueue:
		return errQueueFull
	case c.busy:
		c.queue = append(c.queue, m)
		return nil
	}
	c.busy = true
	if !s.spawn(func() { s.work(key, c, []M{m}) }) {
		c.busy = false
		s.tidy(key, c)
		return ErrNoSlot
	}
	return nil
}

func (s *Store[M]) work(key string, c *chat[M], ms []M) {
	for len(ms) > 0 {
		s.handle(ms)
		s.mu.Lock()
		ms, c.queue = c.queue, nil
		if len(ms) == 0 {
			c.busy = false
			s.tidy(key, c)
		}
		s.mu.Unlock()
	}
}

// Repeat reports whether the chat saw message key id within window, and
// records it.
func (s *Store[M]) Repeat(key, id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.get(key, true)
	if c == nil {
		return false
	}
	now := s.now()
	for k, t := range c.recent {
		if now.Sub(t) >= window {
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
func (s *Store[M]) History(key string) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.get(key, false); c != nil {
		return unexpired(c.history, s.now())
	}
	return nil
}

// Add appends entries to the chat's history, to be forgotten maxAge after
// start (when the message arrived), or disappearing after it if that is
// shorter and not 0. Only the latest exchanges are kept.
func (s *Store[M]) Add(key string, start time.Time, disappearing time.Duration, entries ...Entry) {
	if disappearing <= 0 {
		disappearing = maxAge
	}
	expires := start.Add(min(disappearing, maxAge))
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.get(key, true)
	if c == nil {
		return
	}
	h := unexpired(c.history, s.now())
	for _, e := range entries {
		if goneFrom(c.gone, e) { // deleted or edited while it was answered
			continue
		}
		e.Expires = expires
		h = append(h, e)
	}
	if n := s.messages; len(h) > n {
		h = append([]Entry(nil), h[len(h)-keep(n):]...)
	}
	c.history = h
	s.tidy(key, c)
}

// Forget drops the versions of message id before at, from history and
// from what is added later: one deleted for everyone (at after every
// version), or replaced by an edit made at at. A message still waiting or
// being answered is caught when its entry is added; see Gone. An entry
// merged from several messages goes whole; the bot's replies stay.
func (s *Store[M]) Forget(key, id string, at int64) {
	if id == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.get(key, false) // a chat with no state holds nothing to forget, and nothing waits there
	if c == nil {
		return
	}
	if at <= c.gone[id] {
		return
	}
	if c.gone == nil {
		c.gone = map[string]int64{}
	}
	if len(c.gone) >= maxGone {
		for k := range c.gone {
			delete(c.gone, k)
			break
		}
	}
	c.gone[id] = at
	c.history = slices.DeleteFunc(c.history, func(e Entry) bool { return goneFrom(c.gone, e) })
	s.tidy(key, c)
}

// Retag replaces the ref named id, in the entries holding it, with refs:
// for a reply kept before it was sent, whose timestamps came after.
func (s *Store[M]) Retag(key, id string, refs []Ref) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.get(key, false)
	if c == nil {
		return
	}
	for i, e := range c.history {
		if j := slices.IndexFunc(e.Refs, func(r Ref) bool { return r.ID == id }); j >= 0 {
			c.history[i].Refs = slices.Concat(e.Refs[:j:j], refs, e.Refs[j+1:])
		}
	}
}

// Gone reports whether the version of message id at at was deleted or
// replaced by an edit, so it is not answered or kept.
func (s *Store[M]) Gone(key, id string, at int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.get(key, false)
	return c != nil && id != "" && at < c.gone[id]
}

// goneFrom reports whether e holds a version gone says is deleted or
// replaced.
func goneFrom(gone map[string]int64, e Entry) bool {
	return slices.ContainsFunc(e.Refs, func(r Ref) bool { return r.At < gone[r.ID] })
}

func unexpired(h []Entry, now time.Time) []Entry {
	return slices.DeleteFunc(slices.Clone(h), func(e Entry) bool { return !now.Before(e.Expires) })
}

// Learn records that @name in the chat means the user id.
func (s *Store[M]) Learn(key, name, id string) {
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
func (s *Store[M]) Names(key string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.get(key, false); c != nil {
		return maps.Clone(c.names)
	}
	return nil
}

// ForgetNames drops the chat's names, as when a group's members change.
func (s *Store[M]) ForgetNames(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.get(key, false); c != nil {
		c.names = nil
	}
}

// FollowUp is the replies left that offer tool group g in the chat.
func (s *Store[M]) FollowUp(key string, g int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.get(key, false); c != nil {
		if len(c.history) > 0 && len(unexpired(c.history, s.now())) == 0 {
			s.tidy(key, c) // history that lapsed ends its follow-ups, whether or not its timer has fired
		}
		return c.followUps[g]
	}
	return 0
}

// SetFollowUp sets the replies left that offer tool group g in the chat.
func (s *Store[M]) SetFollowUp(key string, g, n int) {
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

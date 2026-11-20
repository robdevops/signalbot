package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robdevops/signalbot/cmd/geoguess/geo"
	"github.com/robdevops/signalbot/internal/kit/state"
)

// server is GeoGuess: the chats' rounds, their scores, the outbox the bot
// reads, and the photos kept ready.
type server struct {
	cfg       config
	clock     clock
	photos    finder
	log       *log.Logger
	doc       *state.Doc
	nonce     string        // this run's, in each outbox cursor and op ID
	photoWait time.Duration // how long /geo waits for a photo when none is ready

	out   *wire
	dirty chan struct{} // the outbox gained ops: tell the client
	wake  chan struct{} // the prefetcher has work

	mu         sync.Mutex
	rounds     map[string]*round  // by chat
	timers     map[string]stopper // each round's, by chat
	used       []string           // the last maxUsed photos' keys, oldest first
	scores     map[string][]score // by chat, the last scoreDays days
	outbox     []held             // ops the bot hasn't read past, oldest first
	seq        int64              // the last op's sequence number
	subscribed bool               // the client wants to hear of the outbox
	buffer     []geo.Photo        // photos ready for a round, oldest first
	wants      map[string]bool    // regions a /geo asked for that the buffer lacks
	changed    chan struct{}      // closed when the buffer gains a photo
	seeds      map[string]*geo.Seeds

	saveMu sync.Mutex // one save at a time, so the last one written is the newest
}

// round is one chat's round. Its guesses are distances only: where anyone
// guessed is never kept.
type round struct {
	ID       string           `json:"id"` // also the watch's
	Chat     string           `json:"chat"`
	Starter  string           `json:"starter"` // who may end or skip it
	Started  time.Time        `json:"started"`
	Deadline time.Time        `json:"deadline"`
	Answer   geo.Point        `json:"answer"`
	Place    string           `json:"place"`
	Credit   credit           `json:"credit"`
	Guesses  map[string]guess `json:"guesses"` // by player
}

// guess is how far one player's last guess was from the answer, and when
// it was sent.
type guess struct {
	Km float64   `json:"km"`
	At time.Time `json:"at"`
}

// credit names a photo's author, source and licence, with a link to it.
type credit struct {
	Creator string `json:"creator"`
	Source  string `json:"source"`
	Licence string `json:"licence"`
	Link    string `json:"link"`
}

// score is a player's points from one round.
type score struct {
	User   string    `json:"user"`
	Points int       `json:"points"`
	At     time.Time `json:"at"`
}

// saved is geo.json.
type saved struct {
	Rounds []*round           `json:"rounds"`
	Used   []string           `json:"used"`
	Scores map[string][]score `json:"scores"`
	Outbox []held             `json:"outbox"`
}

// The state's limits: photos remembered as used, days of scores, and the
// slack on a round's end.
const (
	maxUsed   = 1000
	scoreDays = 7
	// settle is how long after its deadline a round is revealed, so a guess
	// sent in time that waited in the bot's queue still counts.
	settle = 3 * time.Second
	// watchSlack is how long after the deadline the bot keeps the watch, so a
	// late guess still stays out of the chat's history.
	watchSlack = 5 * time.Second
)

// newServer loads the state saved in cfg.StateDir, re-arms its rounds'
// timers and reveals at once those whose time passed while it was down.
func newServer(cfg config, clk clock, photos finder, logger *log.Logger) (*server, error) {
	s := &server{
		cfg: cfg, clock: clk, photos: photos, log: logger,
		doc:       state.Open(cfg.StateDir, "geo"),
		nonce:     randomID(4),
		photoWait: 10 * time.Second,
		dirty:     make(chan struct{}, 1),
		wake:      make(chan struct{}, 1),
		rounds:    map[string]*round{},
		timers:    map[string]stopper{},
		scores:    map[string][]score{},
		wants:     map[string]bool{},
		changed:   make(chan struct{}),
		seeds:     map[string]*geo.Seeds{},
	}
	var st saved
	if _, err := s.doc.Load(&st); err != nil {
		return nil, err
	}
	s.used = st.Used
	if st.Scores != nil {
		s.scores = st.Scores
	}
	for _, h := range st.Outbox {
		s.seq++
		h.seq = s.seq
		s.outbox = append(s.outbox, h)
	}
	s.mu.Lock()
	revealed := 0
	for _, r := range st.Rounds {
		if r == nil || s.rounds[r.Chat] != nil {
			continue
		}
		if r.Guesses == nil {
			r.Guesses = map[string]guess{}
		}
		s.rounds[r.Chat] = r
		if !s.clock.Now().Before(r.Deadline.Add(settle)) {
			s.timeUpLocked(r)
			revealed++
		} else {
			s.arm(r)
		}
	}
	s.mu.Unlock()
	s.log.Printf("started: %d rounds open, %d revealed late, %d ops held", len(s.rounds), revealed, len(s.outbox))
	if revealed > 0 {
		s.poke()
		s.save()
	}
	return s, nil
}

// save writes geo.json, logging a failure; the state stays in memory, and
// the next change tries again.
func (s *server) save() {
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	s.mu.Lock()
	st := saved{Used: s.used, Scores: s.scores, Outbox: s.outbox, Rounds: make([]*round, 0, len(s.rounds))}
	for _, r := range s.rounds {
		st.Rounds = append(st.Rounds, r)
	}
	slices.SortFunc(st.Rounds, func(a, b *round) int { return strings.Compare(a.Chat, b.Chat) })
	data, err := json.Marshal(st)
	s.mu.Unlock()
	if err == nil {
		err = s.doc.Save(json.RawMessage(data))
	}
	if err != nil {
		s.log.Printf("saving failed: %v", err)
	}
}

// markUsed remembers a photo's key, forgetting the oldest past maxUsed.
// s.mu is held.
func (s *server) markUsed(key string) {
	s.used = append(s.used, key)
	if over := len(s.used) - maxUsed; over > 0 {
		s.used = slices.Delete(s.used, 0, over)
	}
}

// isUsed reports whether a photo's key is among those used recently.
func (s *server) isUsed(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Contains(s.used, key)
}

// randomID is n random bytes in hex.
func randomID(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// validID reports whether id is one of signalbot's pseudonyms with prefix
// (c_ or u_), so it can go into a {u_…} placeholder safely.
func validID(id, prefix string) bool {
	rest, ok := strings.CutPrefix(id, prefix)
	if !ok || rest == "" || len(rest) > 100 {
		return false
	}
	for _, c := range rest {
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// held is an op waiting for the bot to read past it.
type held struct {
	Op     op        `json:"op"`
	Queued time.Time `json:"queued"`
	seq    int64     // this run's, which cursors count in
}

// op is one thing the bot is asked to do: post in a chat, or end a watch.
type op struct {
	ID    string `json:"id"`
	Op    string `json:"op"` // post or unwatch
	Chat  string `json:"chat"`
	Text  string `json:"text,omitempty"`
	Watch string `json:"watch,omitempty"`
}

// outboxPage is what reading the outbox returns.
type outboxPage struct {
	Cursor string `json:"cursor"`
	More   bool   `json:"more"`
	Ops    []op   `json:"ops"`
}

// The outbox's limits: how long an op unread is kept, and the most a page
// holds, well under the 4 MiB line signalbot reads.
const (
	keepOps  = time.Hour
	pageSize = 3 << 20
)

// queue adds an op to the outbox. s.mu is held; the caller pokes the
// client and saves once it is released.
func (s *server) queue(o op) {
	s.seq++
	o.ID = s.nonce + "." + strconv.FormatInt(s.seq, 10)
	s.outbox = append(s.outbox, held{Op: o, Queued: s.clock.Now(), seq: s.seq})
}

// readOutbox is the page after cursor. A cursor of this run lets go of the
// ops up to it, which the bot has read; any other, such as one from before
// a restart, gets everything still held.
func (s *server) readOutbox(cursor string) outboxPage {
	s.mu.Lock()
	after, known := s.cursor(cursor)
	now := s.clock.Now()
	n := len(s.outbox)
	s.outbox = slices.DeleteFunc(s.outbox, func(h held) bool {
		return known && h.seq <= after || now.Sub(h.Queued) > keepOps
	})
	changed := len(s.outbox) != n
	last := s.seq
	if known {
		last = after
	}
	page := outboxPage{Ops: []op{}}
	size := 0
	for _, h := range s.outbox {
		data, _ := json.Marshal(h.Op)
		if len(page.Ops) > 0 && size+len(data) > pageSize {
			page.More = true
			break
		}
		size += len(data)
		page.Ops = append(page.Ops, h.Op)
		last = h.seq
	}
	page.Cursor = s.nonce + "." + strconv.FormatInt(last, 10)
	s.mu.Unlock()
	if changed {
		s.save()
	}
	return page
}

// cursor reads a cursor of this run: the sequence number read up to.
// s.mu is held.
func (s *server) cursor(c string) (int64, bool) {
	nonce, seq, ok := strings.Cut(c, ".")
	if !ok || nonce != s.nonce {
		return 0, false
	}
	n, err := strconv.ParseInt(seq, 10, 64)
	if err != nil || n < 0 || n > s.seq {
		return 0, false
	}
	return n, true
}

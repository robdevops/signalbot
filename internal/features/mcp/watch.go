package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// A watch's limits.
const (
	maxWatches = 8       // live in one chat, per addon
	maxPattern = 1 << 10 // a pattern's size, in bytes
	maxWatchID = 64      // an ID's size, in bytes
	maxGuard   = 500     // a guard's size, in runes
	maxMatches = 256     // matched messages waiting for the watch worker, per addon
	claimLife  = 48 * time.Hour
	leaseLife  = 24 * time.Hour
	tombLife   = 10 * time.Minute
)

// watchRate caps the watch worker's calls in each chat; a match past it
// waits its turn. Tests change it.
var watchRate = rateConfig{PerMinute: 120, Burst: 20}

// watchSpec is the watch a direct command's result asks for, in its _meta.
type watchSpec struct {
	ID      string `json:"id"`
	Pattern string `json:"pattern"` // RE2
	Until   string `json:"until"`   // RFC 3339
	Tool    string `json:"tool"`    // called with each message it matches
	Guard   string `json:"guard"`   // optional: the answer, while it lasts, to a message addressed to the bot that quotes its pictures
}

// watch is a live watch: an addon's claim, until Until, on the messages
// matching Pattern in one chat that the bot gets while it lasts, and the
// edits of those sent from Since on.
type watch struct {
	ID       string    `json:"id"`
	Chat     string    `json:"chat"` // the chat's pseudonym; its lease names the chat
	Pattern  string    `json:"pattern"`
	Tool     string    `json:"tool"`
	Guard    string    `json:"guard,omitempty"`
	Since    int64     `json:"since"` // the Timestamp of the command that made it, by its sender's clock: only for edits
	Until    time.Time `json:"until"`
	Pictures []int64   `json:"pictures,omitempty"` // when the bot's messages with the command's pictures were sent, as Quote.Sent names them

	key string         // the chat's key
	re  *regexp.Regexp // Pattern
}

// claimKey names a message a watch took: its chat's key, its sender's
// pseudonym and when it was sent.
type claimKey struct {
	Chat   string `json:"chat"`
	Sender string `json:"sender"`
	Sent   int64  `json:"sent"`
}

// claim is a message a watch took, whose edits go to the same tool.
type claim struct {
	claimKey
	Tool    string    `json:"tool"`
	Got     int64     `json:"got"` // when the bot got it, in Unix seconds
	Expires time.Time `json:"expires"`
}

// lease is a chat an addon may post and react in, until it expires.
type lease struct {
	Chat    feature.Chat `json:"chat"`
	Expires time.Time    `json:"expires"`
}

// match is a message a watch took, for the watch worker.
type match struct {
	msg   feature.Message
	tool  string
	at    time.Time // when the bot got it
	first int64     // for an edit, when the bot got the original, in Unix seconds; 0 if unknown
	due   time.Time // when the worker may call its tool, within its chat's rate
	tries int       // calls that failed with the server down
}

// A match whose call finds the server down waits for it to start again, up
// to downWait, and is dropped after maxTries such calls.
const (
	downWait = time.Minute
	maxTries = 3
)

// lease lets the addon post in chat for the next leaseLife.
func (a *addon) lease(chat feature.Chat) {
	id := a.chatID(chat)
	a.mu.Lock()
	a.leases[id] = lease{chat, a.now().Add(leaseLife)}
	a.mu.Unlock()
	a.changed()
}

// watch makes the watch spec asks for in m's chat, after the command m,
// and returns it, or nil if it is refused, which is logged without the
// pattern.
func (a *addon) watch(m feature.Message, spec watchSpec) *watch {
	refuse := func(why string) *watch {
		logx.Warnf("MCP server %s: a watch refused: %s", a.server, why)
		return nil
	}
	now := a.now()
	until, err := time.Parse(time.RFC3339, spec.Until)
	var re *regexp.Regexp
	switch {
	case !a.cfg.Watch:
		return refuse("the addon block doesn't allow watch")
	case spec.ID == "" || len(spec.ID) > maxWatchID:
		return refuse("its id is empty or too long")
	case !a.bot[spec.Tool]:
		return refuse("its tool isn't one the bot may call")
	case len(spec.Pattern) > maxPattern:
		return refuse("its pattern is over 1 KB")
	case err != nil:
		return refuse("its until isn't an RFC 3339 time")
	case !until.After(now):
		return refuse("its until has passed")
	}
	if re, err = regexp.Compile(spec.Pattern); err != nil {
		return refuse("its pattern isn't RE2")
	}
	if ordinary(re) {
		return refuse("its pattern matches ordinary chat")
	}
	if latest := now.Add(a.cfg.watchMax); until.After(latest) {
		until = latest
	}
	w := &watch{ID: spec.ID, Chat: a.chatID(m.Chat), Pattern: spec.Pattern, Tool: spec.Tool, Guard: txt.Clamp(strings.TrimSpace(spec.Guard), maxGuard),
		Since: m.Timestamp, Until: until, key: m.Chat.Key(), re: re}
	a.mu.Lock()
	tomb := w.Chat + "\x00" + w.ID
	if t, ok := a.tombs[tomb]; ok && now.Before(t) {
		delete(a.tombs, tomb)
		a.mu.Unlock()
		return refuse("the addon ended it before it began")
	}
	live := slices.DeleteFunc(a.watches[w.key], func(o *watch) bool { return o.ID == w.ID || !now.Before(o.Until) })
	if len(live) >= maxWatches {
		a.watches[w.key] = live
		a.mu.Unlock()
		return refuse("8 watches are already live in the chat")
	}
	a.watches[w.key] = append(live, w)
	a.mu.Unlock()
	a.changed()
	logx.Infof("MCP server %s: watching a chat for %s", a.server, until.Sub(now).Round(time.Second))
	return w
}

// guarding records that w's command's pictures went out in the messages
// sent at sent, so its guard knows a reply to them.
func (a *addon) guarding(w *watch, sent []int64) {
	a.mu.Lock()
	w.Pictures = append(w.Pictures, sent...)
	a.mu.Unlock()
	a.changed()
}

// claim is the Action for m if one of the addon's watches takes it: Done,
// with m sent to the watch worker. So is an edit of a message one took. An
// addressed message that quotes the pictures of a watch with a guard, even
// with no text, gets the guard. It runs on the network's reader goroutine
// for an unaddressed message, so it changes only what is in memory, and
// never waits.
//
// An original is taken whenever the bot gets it while a watch lasts, not by
// its Timestamp: on Signal that is its sender's clock, which may be behind.
// An edit's original must be from Since on, so an older message's edit is
// never taken.
func (a *addon) claim(m feature.Message) feature.Action {
	if !a.allows(m.Chat) {
		return feature.Action{}
	}
	now := a.now()
	key := m.Chat.Key()
	a.mu.Lock()
	live := slices.Clone(a.watches[key]) // their patterns never change
	a.mu.Unlock()
	if m.Text == "" || len(live) == 0 && m.Edited == 0 {
		return a.guard(m, live, now)
	}
	c := claimKey{key, a.userID(m.Chat, m.Sender), m.Timestamp}
	got := match{msg: m, at: now}
	if m.Edited != 0 {
		a.mu.Lock()
		if old, ok := a.claims[c]; ok && now.Before(old.Expires) {
			got.tool, got.first = old.Tool, old.Got
		}
		a.mu.Unlock()
	}
	for _, w := range live {
		if got.tool == "" && now.Before(w.Until) && (m.Edited == 0 || m.Timestamp >= w.Since) && w.re.MatchString(m.Text) {
			got.tool = w.Tool
		}
	}
	if got.tool == "" {
		return a.guard(m, live, now)
	}
	a.mu.Lock()
	if _, ok := a.claims[c]; !ok {
		a.claims[c] = claim{c, got.tool, now.Unix(), now.Add(claimLife)}
	}
	a.leases[a.chatID(m.Chat)] = lease{m.Chat, now.Add(leaseLife)}
	a.mu.Unlock()
	a.changed()
	select {
	case a.matches <- got:
	default:
		logx.Warnf("addon %s: watch queue full, a matched message dropped", a.server)
	}
	return feature.Action{Done: true}
}

// guard is the guard of a watch in live, live at now, if m is addressed to
// the bot and quotes its pictures.
func (a *addon) guard(m feature.Message, live []*watch, now time.Time) feature.Action {
	if !m.Addressed || m.Quote == nil || !m.Quote.Bot || m.Quote.Sent == 0 {
		return feature.Action{}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, w := range live {
		if w.Guard != "" && now.Before(w.Until) && slices.Contains(w.Pictures, m.Quote.Sent) {
			return feature.Action{Answer: w.Guard}
		}
	}
	return feature.Action{}
}

// work passes each matched message to its watch's tool, until ctx ends: in
// each chat in turn and within the chat's rate, a match past the rate
// waiting its turn without holding up other chats'. A match whose call finds
// the server down is kept, and goes first once it has started again.
func (a *addon) work(ctx context.Context) {
	var queue []match // oldest first; a chat's are due in order
	var down <-chan struct{}
	for ctx.Err() == nil {
		in := a.matches
		if len(queue) >= maxMatches {
			in = nil // claim drops what can't wait
		}
		var wake <-chan time.Time
		switch {
		case down != nil:
			wake = time.After(downWait)
		case len(queue) > 0:
			i := 0
			for j, m := range queue {
				if m.due.Before(queue[i].due) {
					i = j
				}
			}
			if d := time.Until(queue[i].due); d > 0 {
				wake = time.After(d)
				break
			}
			m := queue[i]
			queue = slices.Delete(queue, i, i+1)
			select {
			case <-a.up: // a start before this call
			default:
			}
			if a.forward(ctx, m) {
				if m.tries++; m.tries < maxTries {
					queue, down = slices.Insert(queue, 0, m), a.up
				} else {
					logx.Warnf("addon %s: a matched message dropped: the server was down for %d calls", a.server, maxTries)
				}
			}
			continue
		}
		select {
		case <-ctx.Done():
			return
		case m := <-in:
			m.due = time.Now().Add(a.calls.take(m.msg.Chat.Key(), a.now()))
			queue = append(queue, m)
		case <-down:
			down = nil
		case <-wake:
			down = nil
		}
	}
}

// forward calls m's tool with its text and context, and puts the reaction
// the result asks for on it, and reports whether the call found the server
// down. Only the call's outcome, time and size are logged.
func (a *addon) forward(ctx context.Context, m match) (down bool) {
	c := a.context(m.msg, m.at)
	if m.first != 0 {
		c.Sent = m.first
	}
	ctx, cancel := context.WithTimeout(ctx, directTimeout)
	defer cancel()
	start := time.Now()
	res, err := a.conn.call(ctx, "tools/call", map[string]any{
		"name":      m.tool,
		"arguments": map[string]string{"text": m.msg.Text},
		"_meta":     map[string]any{vendor + "context": c},
	})
	var r toolResult
	outcome := "ok"
	switch {
	case err != nil:
		outcome = errCode(err)
	case json.Unmarshal(res, &r) != nil:
		outcome = "a malformed result"
	case r.IsError:
		outcome = "an error result"
	}
	logx.Infof("MCP server %s: a watch called %s: %s in %dms, %d bytes", a.server, m.tool, outcome, time.Since(start).Milliseconds(), len(res))
	if outcome == "ok" && r.Meta.React != "" {
		msg := feature.Message{Chat: m.msg.Chat, Sender: m.msg.Sender, Timestamp: m.msg.Timestamp, Edited: m.msg.Edited} // without its text
		if why := a.react(msg, r.Meta.React); why != "" {
			logx.Warnf("addon %s: a reaction dropped: %s", a.server, why)
		}
	}
	return errors.Is(err, errNotRunning) || errors.Is(err, errExited)
}

// react puts emoji on msg, if the addon may, and returns why not if not.
func (a *addon) react(msg feature.Message, emoji string) (why string) {
	switch {
	case !a.cfg.React:
		return "the addon block doesn't allow react"
	case !txt.SingleEmoji(emoji):
		return "not one emoji"
	case !a.posts.allow(msg.Chat.Key(), a.now()):
		return "over the rate limit"
	}
	a.host.React(msg.Chat, msg, emoji)
	return ""
}

// limiter is a token bucket for each chat: rate.PerMinute tokens a minute,
// up to rate.Burst.
type limiter struct {
	every time.Duration // a token comes back each
	burst float64

	mu      sync.Mutex
	buckets map[string]*bucket // by chat key
}

type bucket struct {
	tokens float64
	at     time.Time // when tokens was counted
}

func newLimiter(rate rateConfig) *limiter {
	return &limiter{every: time.Minute / time.Duration(max(rate.PerMinute, 1)), burst: float64(max(rate.Burst, 1)), buckets: map[string]*bucket{}}
}

// fill is chat's bucket at now, with what came back since. l.mu is held.
func (l *limiter) fill(chat string, now time.Time) *bucket {
	if len(l.buckets) > 1000 {
		for k, b := range l.buckets {
			if b.tokens+float64(now.Sub(b.at))/float64(l.every) >= l.burst {
				delete(l.buckets, k) // full: the same as none
			}
		}
	}
	b := l.buckets[chat]
	if b == nil {
		b = &bucket{l.burst, now}
		l.buckets[chat] = b
	}
	b.tokens = min(l.burst, b.tokens+float64(now.Sub(b.at))/float64(l.every))
	b.at = now
	return b
}

// allow takes a token for chat, if there is one.
func (l *limiter) allow(chat string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.fill(chat, now)
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// take takes a token for chat, owing it if there is none, and returns how
// long to wait until it is due.
func (l *limiter) take(chat string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.fill(chat, now)
	b.tokens--
	if b.tokens >= 0 {
		return 0
	}
	return time.Duration(-b.tokens * float64(l.every))
}

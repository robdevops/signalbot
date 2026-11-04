package signal

import (
	"strconv"
	"sync"
	"time"
)

// recentMax caps the messages recent remembers, and recentAge how long, so
// a reply can fetch the original files of the message it quotes: signal-cli
// gives a quote only thumbnails.
const (
	recentMax = 2000
	recentAge = 24 * time.Hour
)

// recent remembers the attachments of messages received lately, by chat,
// author and timestamp: their IDs, names, types and sizes, never content,
// and only in memory.
type recent struct {
	mu    sync.Mutex
	byKey map[string]recentEntry
	order []string // keys, oldest first
	now   func() time.Time
}

type recentEntry struct {
	at          time.Time
	attachments []attachment
}

func recentKey(chat, author string, ts int64) string {
	return chat + "|" + author + "|" + strconv.FormatInt(ts, 10)
}

// add remembers a message's attachments, dropping the oldest beyond
// recentMax. It never blocks for long, so the reader can call it.
func (r *recent) add(chat, author string, ts int64, as []attachment) {
	if author == "" || ts == 0 || len(as) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byKey == nil {
		r.byKey = map[string]recentEntry{}
	}
	key := recentKey(chat, author, ts)
	if _, seen := r.byKey[key]; !seen {
		r.order = append(r.order, key)
	}
	r.byKey[key] = recentEntry{r.clock(), as}
	for len(r.order) > recentMax {
		delete(r.byKey, r.order[0])
		r.order = r.order[1:]
	}
}

// get returns the attachments of the message by author at ts in chat, if
// remembered and not older than recentAge.
func (r *recent) get(chat, author string, ts int64) ([]attachment, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byKey[recentKey(chat, author, ts)]
	if !ok || r.clock().Sub(e.at) > recentAge {
		return nil, false
	}
	return e.attachments, true
}

func (r *recent) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

package signal

import (
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// recentMax caps the messages recent remembers, recentBytes the pictures
// the bot sent that it keeps, and recentAge how long, so a reply can fetch
// the original files of the message it quotes: signal-cli gives a quote
// only thumbnails.
const (
	recentMax   = 2000
	recentBytes = 32 << 20
	recentAge   = 24 * time.Hour
)

// sentPrefix starts the ID of a picture the bot sent, which recent holds
// itself: signal-cli has no ID for an attachment it sent.
const sentPrefix = "sent:"

// recent remembers the attachments of messages received lately, by chat,
// author and timestamp: their IDs, names, types and sizes, never content,
// and only in memory; and the pictures the bot sent, content and all, as
// signal-cli keeps no copy to fetch.
type recent struct {
	mu    sync.Mutex
	byKey map[string]recentEntry
	order []string // keys, oldest first
	bytes int      // of the pictures held
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
	if old, seen := r.byKey[key]; !seen {
		r.order = append(r.order, key)
	} else {
		r.bytes -= held(old.attachments)
	}
	r.byKey[key] = recentEntry{r.clock(), as}
	r.bytes += held(as)
	r.prune()
}

// held is the bytes of the pictures in as.
func held(as []attachment) int {
	n := 0
	for _, a := range as {
		n += len(a.data)
	}
	return n
}

// sent returns a picture the bot sent in chat, by its ID: only from that
// chat, as two chats' sends can share a timestamp.
func (r *recent) sent(chat, id string) ([]byte, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prune()
	for k, e := range r.byKey {
		if !strings.HasPrefix(k, chat+"|") {
			continue
		}
		for _, a := range e.attachments {
			if a.ID == id && a.data != nil {
				return a.data, true
			}
		}
	}
	return nil, false
}

// forget drops the attachments of the message by author at ts in chat,
// deleted for everyone.
func (r *recent) forget(chat, author string, ts int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := recentKey(chat, author, ts)
	if e, ok := r.byKey[key]; ok {
		r.bytes -= held(e.attachments)
		delete(r.byKey, key)
		r.order = slices.DeleteFunc(r.order, func(k string) bool { return k == key })
	}
}

// prune drops the oldest beyond recentMax, and any older than recentAge,
// so nothing is remembered longer than a day; and beyond recentBytes the
// oldest pictures the bot sent, leaving others' records. r.mu is held.
func (r *recent) prune() {
	now := r.clock()
	for len(r.order) > 0 && (len(r.order) > recentMax || now.Sub(r.byKey[r.order[0]].at) > recentAge) {
		r.bytes -= held(r.byKey[r.order[0]].attachments)
		delete(r.byKey, r.order[0])
		r.order = r.order[1:]
	}
	for i := 0; r.bytes > recentBytes && i < len(r.order); {
		k := r.order[i]
		if n := held(r.byKey[k].attachments); n > 0 {
			r.bytes -= n
			delete(r.byKey, k)
			r.order = slices.Delete(r.order, i, i+1)
			continue
		}
		i++
	}
}

// get returns the attachments of the message by author at ts in chat, if
// remembered and not older than recentAge.
func (r *recent) get(chat, author string, ts int64) ([]attachment, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.prune()
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

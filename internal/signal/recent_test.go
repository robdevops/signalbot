package signal

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/feature"
)

// recent remembers attachments by chat, author and timestamp, for a day, at
// most recentMax messages, dropping the oldest.
func TestRecent(t *testing.T) {
	now := time.Now()
	r := &recent{now: func() time.Time { return now }}
	pdf := []attachment{{ContentType: "application/pdf", ID: "p1", Filename: "a.pdf", Size: 10}}
	r.add("g:x", "alice", 1, pdf)
	r.add("g:x", "", 2, pdf)      // no author: not kept
	r.add("g:x", "alice", 0, pdf) // no timestamp: not kept
	r.add("g:x", "alice", 3, nil) // nothing to keep
	if got, ok := r.get("g:x", "alice", 1); !ok || !reflect.DeepEqual(got, pdf) {
		t.Fatalf("got %v %v", got, ok)
	}
	for _, miss := range []struct {
		chat, author string
		ts           int64
	}{{"g:y", "alice", 1}, {"g:x", "bob", 1}, {"g:x", "alice", 2}, {"g:x", "", 2}, {"g:x", "alice", 3}} {
		if _, ok := r.get(miss.chat, miss.author, miss.ts); ok {
			t.Errorf("found %+v", miss)
		}
	}
	now = now.Add(recentAge + time.Second)
	if _, ok := r.get("g:x", "alice", 1); ok {
		t.Error("found after a day")
	}
	for i := range recentMax + 5 {
		r.add("g:x", "bob", int64(100+i), pdf)
	}
	if len(r.byKey) != recentMax || len(r.order) != recentMax {
		t.Fatalf("%d kept, %d in order", len(r.byKey), len(r.order))
	}
	if _, ok := r.get("g:x", "bob", 100); ok {
		t.Error("the oldest wasn't dropped")
	}
	if _, ok := r.get("g:x", "bob", int64(100+recentMax+4)); !ok {
		t.Error("the newest was dropped")
	}
}

// A reply to a message received lately gets its original files and
// full-size images, not the quote's thumbnails; to one not remembered, the
// thumbnails, and its files noted with no ID. Only group messages and DMs
// from allowed senders are remembered.
func TestQuotedOriginals(t *testing.T) {
	captureLogs(t)
	c := newClient("x", map[string]bool{bobID: true}, nil)
	c.closed.Store(true)
	connected(c)
	original := `{` + fromBob + `,"timestamp":1700,"dataMessage":{"message":"the report",` + inGroup + `,"attachments":[` +
		`{"contentType":"application/pdf","id":"pdf1","filename":"report.pdf","size":900},{"contentType":"image/png","id":"img1","size":800}]}}`
	c.event(parse(t, original))
	reply := func(ts int) feature.Message {
		e := parse(t, fmt.Sprintf(`{"sourceUuid":"%s","timestamp":1800,"dataMessage":{"message":"￼ summarise",%s,%s,`+
			`"quote":{"id":%d,"authorUuid":"%s","text":"the report","attachments":[{"contentType":"application/pdf","filename":"report.pdf"},`+
			`{"contentType":"image/png","thumbnail":{"contentType":"image/png","id":"thumb1","size":5}}]}}}`, aliceID, atBot, inGroup, ts, bobID))
		return c.message(e, botID)
	}
	if got, want := reply(1700).Media, []feature.Media{{ID: "img1", Quoted: true},
		{ID: "pdf1", Quoted: true, File: true, Name: "report.pdf", MIME: "application/pdf", Size: 900}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("remembered: %+v", got)
	}
	if got, want := reply(1699).Media, []feature.Media{{ID: "thumb1", Quoted: true},
		{Quoted: true, File: true, Name: "report.pdf", MIME: "application/pdf"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("not remembered: %+v", got)
	}
	// A DM from someone not allowed is not remembered.
	c.event(parse(t, `{"sourceUuid":"`+aliceID+`","timestamp":1900,"dataMessage":{"attachments":[{"contentType":"application/pdf","id":"x","size":1}]}}`))
	if _, ok := c.recent.get("u:"+aliceID, aliceID, 1900); ok {
		t.Error("a stranger's DM was remembered")
	}
}

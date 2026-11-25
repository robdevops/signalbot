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
	if _, ok := r.get("g:x", "alice", 1); ok || len(r.byKey) != 0 || len(r.order) != 0 {
		t.Errorf("kept after a day: %d", len(r.byKey))
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
	c := newClient("x", only(bobID), nil)
	c.closed.Store(true)
	connected(c)
	original := `{` + fromBob + `,"timestamp":1700,"dataMessage":{"message":"the report",` + inGroup + `,"attachments":[` +
		`{"contentType":"application/pdf","id":"pdf1","filename":"report.pdf","size":900},{"contentType":"image/png","id":"img1","size":800}]}}`
	c.event(parse(t, original))
	reply := func(ts int) feature.Message {
		e := parse(t, fmt.Sprintf(`{"sourceUuid":"%s","timestamp":1800,"dataMessage":{"message":"￼ summarise",%s,%s,`+
			`"quote":{"id":%d,"authorUuid":"%s","text":"the report","attachments":[{"contentType":"application/pdf","filename":"report.pdf"},`+
			`{"contentType":"image/png","thumbnail":{"contentType":"image/png","id":"thumb1","size":5}}]}}}`, aliceID, atBot, inGroup, ts, bobID))
		return c.message(e, botID, false)
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

// Pictures the bot sent are held, content and all, up to recentBytes: the
// oldest go first, and a delete frees its bytes.
func TestRecentSentPictures(t *testing.T) {
	var r recent
	big := make([]byte, recentBytes/3)
	for i := range int64(4) {
		r.add("u:x", "bot", i+1, []attachment{{ID: fmt.Sprintf("sent:%d:0", i+1), ContentType: "image/png", Size: len(big), data: big}})
	}
	if _, ok := r.sent("u:x", "sent:1:0"); ok {
		t.Fatal("the oldest kept past the cap")
	}
	for _, id := range []string{"sent:2:0", "sent:3:0", "sent:4:0"} {
		if d, ok := r.sent("u:x", id); !ok || len(d) != len(big) {
			t.Fatalf("%s not held", id)
		}
	}
	r.forget("u:x", "bot", 4)
	if _, ok := r.sent("u:x", "sent:4:0"); ok || r.bytes != 2*len(big) {
		t.Fatalf("after a delete: %d bytes held", r.bytes)
	}
}

// A picture is served only in the chat it was sent in, though another
// chat's send had the same timestamp; and the byte cap drops only the bot's
// pictures, not others' records.
func TestRecentSentPicturesByChat(t *testing.T) {
	var r recent
	r.add("g:other", "alice", 1, []attachment{{ID: "a1", ContentType: "image/png", Size: 10}})
	r.add("g:A", "bot", 7, []attachment{{ID: "sent:7:0", ContentType: "image/png", Size: 1, data: []byte("A")}})
	r.add("g:B", "bot", 7, []attachment{{ID: "sent:7:0", ContentType: "image/png", Size: 1, data: []byte("B")}})
	for range 20 {
		if d, ok := r.sent("g:B", "sent:7:0"); !ok || string(d) != "B" {
			t.Fatalf("got %q", d)
		}
	}
	if _, ok := r.sent("g:C", "sent:7:0"); ok {
		t.Fatal("served in another chat")
	}
	big := make([]byte, recentBytes)
	r.add("g:A", "bot", 8, []attachment{{ID: "sent:8:0", ContentType: "image/png", Size: len(big), data: big}})
	if _, ok := r.get("g:other", "alice", 1); !ok {
		t.Fatal("the byte cap dropped a record with no bytes")
	}
	if _, ok := r.sent("g:A", "sent:7:0"); ok {
		t.Fatal("an older picture kept past the cap")
	}
}

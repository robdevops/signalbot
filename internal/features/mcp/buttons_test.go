package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	ext "github.com/robdevops/signalbot/addon"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/feature/featuretest"
)

// buttonsAddon is a unit addon on a network that shows buttons, whose
// tool "period" is for the bot alone and whose direct command is /rain,
// posting to h.
func buttonsAddon(cfg addonConfig) (*addon, *time.Time, *featuretest.Host) {
	a, now := unitAddon(cfg)
	a.only["period"], a.bot["period"], a.cmds["rain"] = true, true, true
	a.caps = feature.Caps{Buttons: true, Keyboard: true, Silent: true, EditMedia: true}
	h := featuretest.New()
	a.host = h
	return a, now, h
}

var rainButtons = [][]ext.Button{{{Label: "24h", Data: "rain 24h"}, {Label: "7d", Data: "rain 7d"}}}

// With announce, a lease a direct command made never expires, even when a
// watch's lease renews it, and is saved so; it stays within chats. Without
// announce, or once announce is taken away, a lease lasts leaseLife.
func TestAnnounce(t *testing.T) {
	cfg := everything
	cfg.Announce = true
	a, now, _ := buttonsAddon(cfg)
	a.lease(inA, true)
	a.lease(inB, false)
	a.lease(inA, false) // as a watch renews it
	*now = now.Add(30 * 24 * time.Hour)
	post := func(a *addon, id string, chat feature.Chat) string {
		return a.apply(ext.Op{ID: id, Op: "post", Chat: a.chatID(chat), Text: "Rain soon"})
	}
	if why := post(a, "1", inA); why != "" {
		t.Fatalf("a month on: %q", why)
	}
	if why := post(a, "2", inB); why != "for a chat without a lease" {
		t.Fatalf("a chat a watch leased: %q", why)
	}
	s := a.state()
	if l, ok := s.Leases[a.chatID(inA)]; !ok || !l.Forever {
		t.Fatalf("saved %+v", s.Leases)
	}
	again, later, _ := buttonsAddon(cfg)
	*later = *now
	again.restore(s)
	if why := post(again, "3", inA); why != "" {
		t.Fatalf("after a restart: %q", why)
	}
	cfg.Chats = []string{groupB}
	outside, later, _ := buttonsAddon(cfg)
	*later = *now
	outside.restore(s)
	if why := post(outside, "4", inA); why != "for a chat without a lease" {
		t.Fatalf("outside chats: %q", why)
	}
	cfg = everything // announce taken away
	off, later, _ := buttonsAddon(cfg)
	*later = *now
	off.restore(s)
	if why := post(off, "5", inA); why != "for a chat without a lease" {
		t.Fatalf("without announce: %q", why)
	}
	off.lease(inB, true)
	if l := off.leases[off.chatID(inB)]; l.Forever {
		t.Fatal("a lease for good without announce")
	}
}

// A post's markup goes to the host as checked: silent, buttons with a
// tool for the bot alone, a keyboard; one that breaks the limits is
// dropped, saying why. A network without buttons gets the post without
// them.
func TestOutboxMarkup(t *testing.T) {
	a, now, h := buttonsAddon(everything)
	a.cfg.Rate = rateConfig{60, 60}
	a.posts = newLimiter(a.cfg.Rate)
	a.lease(inA, false)
	id := a.chatID(inA)
	op := func(o ext.Op) string {
		o.Op, o.Chat = "post", id
		if o.Text == "" {
			o.Text = "Rain"
		}
		return a.apply(o)
	}
	keys := [][]ext.Key{{{Label: "🌧 Rain", Command: "/RAIN 7d"}}}
	// Each sent before the next: sent in two messages, a post's buttons
	// are on the last, for 30 days.
	for i, o := range []ext.Op{
		{ID: "s", Silent: true},
		{ID: "b", Buttons: rainButtons, OnPress: "period"},
		{ID: "k", Keyboard: keys},
		{ID: "r", RemoveKeyboard: true},
	} {
		if why := op(o); why != "" {
			t.Fatalf("%s: %q", o.ID, why)
		}
		h.Posts()[i].OnSent([]int64{int64(100 + 2*i), int64(101 + 2*i)}, false)
	}
	posts := h.Posts()
	if len(posts) != 4 || !posts[0].Markup.Silent || len(posts[1].Markup.Buttons) != 1 || posts[1].Markup.Buttons[0][1] != (feature.Button{Label: "7d", Data: "rain 7d"}) ||
		posts[2].Markup.Keyboard[0][0] != (feature.Key{Label: "🌧 Rain", Command: "/RAIN 7d"}) || !posts[3].Markup.NoKeyboard {
		t.Fatalf("posts %+v", posts)
	}
	if p := a.posted["b"]; p.Sent != 102 || p.Last != 103 || p.OnPress != "period" || !p.Expires.Equal(now.Add(buttonsLife)) {
		t.Fatalf("remembered %+v", p)
	}
	if p := a.posted["s"]; p.OnPress != "" || !p.Expires.Equal(now.Add(postedLife)) {
		t.Fatalf("remembered %+v", p)
	}

	two := []ext.Image{{Data: tinyPNG(), MimeType: "image/png"}, {Data: tinyPNG(), MimeType: "image/png"}}
	for _, c := range []struct {
		o   ext.Op
		why string
	}{
		{ext.Op{ID: "1", Buttons: rainButtons}, "posts with buttons without onPress, or onPress without buttons"},
		{ext.Op{ID: "2", OnPress: "period"}, "posts with buttons without onPress, or onPress without buttons"},
		{ext.Op{ID: "3", Buttons: rainButtons, OnPress: "guess"}, "posts with an onPress that isn't one of its tools for the bot alone"},
		{ext.Op{ID: "4", Buttons: rainButtons, OnPress: "period", Images: two}, "posts with buttons or a keyboard and more than one image"},
		{ext.Op{ID: "4k", Keyboard: keys, Images: two}, "posts with buttons or a keyboard and more than one image"},
		{ext.Op{ID: "4r", RemoveKeyboard: true, Images: two}, "posts with buttons or a keyboard and more than one image"},
		{ext.Op{ID: "5", Buttons: rainButtons, OnPress: "period", Keyboard: keys}, "posts with buttons and a keyboard, or a keyboard and removeKeyboard"},
		{ext.Op{ID: "6", Keyboard: keys, RemoveKeyboard: true}, "posts with buttons and a keyboard, or a keyboard and removeKeyboard"},
		{ext.Op{ID: "7", Buttons: slices.Repeat(rainButtons, 6), OnPress: "period"}, "posts with more than 5 rows of 5 buttons or keys, or an empty row"},
		{ext.Op{ID: "8", Buttons: [][]ext.Button{slices.Repeat(rainButtons[0], 3)}, OnPress: "period"}, "posts with more than 5 rows of 5 buttons or keys, or an empty row"},
		{ext.Op{ID: "9", Buttons: [][]ext.Button{{}}, OnPress: "period"}, "posts with more than 5 rows of 5 buttons or keys, or an empty row"},
		{ext.Op{ID: "10", Buttons: [][]ext.Button{{{Label: strings.Repeat("é", 33), Data: "x"}}}, OnPress: "period"}, "posts with a button's label or data empty or too long"},
		{ext.Op{ID: "11", Buttons: [][]ext.Button{{{Label: "x", Data: strings.Repeat("é", 33)}}}, OnPress: "period"}, "posts with a button's label or data empty or too long"},
		{ext.Op{ID: "12", Buttons: [][]ext.Button{{{Label: " ", Data: "x"}}}, OnPress: "period"}, "posts with a button's label or data empty or too long"},
		{ext.Op{ID: "13", Keyboard: [][]ext.Key{{{Label: "Rain", Command: "rain"}}}}, "posts with a key's label empty, too long or twice, or its command not a command"},
		{ext.Op{ID: "14", Keyboard: [][]ext.Key{{{Label: "Rain", Command: "/rain"}, {Label: "Rain", Command: "/rain 7d"}}}}, "posts with a key's label empty, too long or twice, or its command not a command"},
		{ext.Op{ID: "15", Keyboard: [][]ext.Key{{{Label: "Rain", Command: "/rain\n/status"}}}}, "posts with a key's label empty, too long or twice, or its command not a command"},
		{ext.Op{ID: "16", Keyboard: [][]ext.Key{{{Label: "Rain ", Command: "/rain"}}}}, "posts with a key's label empty, too long or twice, or its command not a command"},
		{ext.Op{ID: "17", Keyboard: [][]ext.Key{{{Label: "Forget", Command: "/forget"}}}}, "posts with a key's command not one of its commands"},
		{ext.Op{ID: "18", Keyboard: [][]ext.Key{{{Label: "Rain", Command: "/rainfall"}}}}, "posts with a key's command not one of its commands"},
	} {
		if why := op(c.o); why != c.why {
			t.Errorf("%s: %q, want %q", c.o.ID, why, c.why)
		}
	}
	if n := len(h.Posts()); n != 4 {
		t.Fatalf("%d posts", n)
	}

	// Signal: the buttons left off, and the post remembered as a plain one.
	a.caps = feature.Caps{EditLimit: 10, EditWindow: 24 * time.Hour} // Signal's
	if why := op(ext.Op{ID: "plain", Buttons: rainButtons, OnPress: "period", Keyboard: nil}); why != "" {
		t.Fatal(why)
	}
	p := h.Posts()[4]
	if len(p.Markup.Buttons) != 0 {
		t.Fatalf("buttons on Signal %+v", p.Markup)
	}
	p.OnSent([]int64{200}, false)
	if a.posted["plain"].OnPress != "" {
		t.Fatalf("remembered %+v", a.posted["plain"])
	}
}

// A post with markup is a message of its own, never merged, but silent
// posts merge with each other.
func TestOutboxMarkupNotMerged(t *testing.T) {
	text := func(s string, mk feature.Markup) *outgoing {
		return &outgoing{Post: feature.Post{Text: s, Markup: mk}}
	}
	silent := feature.Markup{Silent: true}
	buttons := feature.Markup{Buttons: [][]feature.Button{{{Label: "a", Data: "a"}}}}
	for _, c := range []struct {
		a, b *outgoing
		fits bool
	}{
		{text("a", silent), text("b", silent), true},
		{text("a", silent), text("b", feature.Markup{}), false},
		{text("a", buttons), text("b", feature.Markup{}), false},
		{text("a", feature.Markup{}), text("b", feature.Markup{Keyboard: [][]feature.Key{{{Label: "a", Command: "/a"}}}}), false},
		{text("a", feature.Markup{}), text("b", feature.Markup{NoKeyboard: true}), false},
		{text("a", feature.Markup{}), &outgoing{Post: feature.Post{Text: "b", Edit: 100}}, false},
	} {
		if got := c.a.fits(c.b); got != c.fits {
			t.Errorf("%+v and %+v: fits %v", c.a.Markup, c.b.Markup, got)
		}
	}
}

// An edit names an earlier post in its chat, and replaces its text, image
// and buttons, as a post: with the post permission, a lease and within the
// rate. Sent anew, as on Signal with an image, the post is the new message
// from then on; its buttons are remembered for 30 days from the edit.
func TestOutboxEdit(t *testing.T) {
	cfg := everything
	cfg.Rate = rateConfig{1, 4}
	a, now, h := buttonsAddon(cfg)
	*now = now.Add(time.Hour)
	a.lease(inA, false)
	a.lease(inB, false)
	idA, idB := a.chatID(inA), a.chatID(inB)
	if why := a.apply(ext.Op{ID: "q", Op: "post", Chat: idA, Text: "Rain, 24 hours"}); why != "" {
		t.Fatal(why)
	}
	h.Posts()[0].OnSent([]int64{100}, false)
	edit := func(id, chat, post string, more func(*ext.Op)) string {
		o := ext.Op{ID: id, Op: "edit", Chat: chat, Post: post, Text: "Rain, 7 days {u_" + strings.Repeat("a", 26) + "}", Buttons: rainButtons, OnPress: "period"}
		if more != nil {
			more(&o)
		}
		return a.apply(o)
	}
	if why := edit("e1", idA, "q", nil); why != "" {
		t.Fatal(why)
	}
	e := h.Posts()[1]
	if e.Edit != 100 || e.Chat != inA || e.Text != "Rain, 7 days someone" || len(e.Markup.Buttons) != 1 || e.Label != "" {
		t.Fatalf("edit %+v", e)
	}
	e.OnSent([]int64{100}, false)
	if p := a.posted["q"]; p.Sent != 100 || p.last() != 100 || p.OnPress != "period" || !p.Expires.Equal(now.Add(buttonsLife)) {
		t.Fatalf("remembered %+v", p)
	}
	if !a.done["e1"] {
		t.Fatal("the edit isn't applied")
	}
	// Sent anew: the post is the new message from then on.
	if why := edit("e2", idA, "q", func(o *ext.Op) { o.Images = []ext.Image{{Data: tinyPNG(), MimeType: "image/png"}} }); why != "" {
		t.Fatal(why)
	}
	h.Posts()[2].OnSent([]int64{300}, false)
	if p := a.posted["q"]; p.Sent != 300 || p.last() != 300 {
		t.Fatalf("remembered %+v", p)
	}
	for _, c := range []struct {
		id, chat, post string
		more           func(*ext.Op)
		why            string
	}{
		{"x1", idA, "nope", nil, "edits of an unknown post"},
		{"x2", idB, "q", nil, "edits of an unknown post"},
		{"x3", idA, "q", func(o *ext.Op) {
			o.Images = slices.Repeat([]ext.Image{{Data: tinyPNG(), MimeType: "image/png"}}, 2)
		}, "edits with more than one image"},
		{"x4", idA, "q", func(o *ext.Op) { o.Text = " " }, "empty edits"},
		{"x5", idA, "q", func(o *ext.Op) { o.OnPress = "guess" }, "edits with an onPress that isn't one of its tools for the bot alone"},
		{"x6", idA, "q", func(o *ext.Op) { o.Keyboard = [][]ext.Key{{{Label: "a", Command: "/a"}}} }, "malformed edits"},
		{"x7", idA, "", nil, "malformed edits"},
	} {
		if why := edit(c.id, c.chat, c.post, c.more); why != c.why {
			t.Errorf("%s: %q, want %q", c.id, why, c.why)
		}
	}
	// Within the rate: the post and two edits took three of four.
	if why := edit("e3", idA, "q", nil); why != "" {
		t.Fatal(why)
	}
	h.Posts()[3].OnSent(nil, false) // refused by the network: the post is as it was
	if why := edit("e4", idA, "q", nil); why != "edits over the rate limit" {
		t.Fatalf("past the rate: %q", why)
	}
	if p := a.posted["q"]; p.Sent != 300 {
		t.Fatalf("after a refused edit %+v", p)
	}
	// Without a lease, or the post permission.
	delete(a.leases, idA)
	if why := edit("e5", idA, "q", nil); why != "for a chat without a lease" {
		t.Fatalf("without a lease: %q", why)
	}
	a.cfg.Post = false
	if why := edit("e6", idA, "q", nil); why != "edits not allowed" {
		t.Fatalf("without post: %q", why)
	}
}

// An edit queued behind another edits the post as the one before left it.
func TestOutboxEditsInTurn(t *testing.T) {
	a, _, h := buttonsAddon(everything)
	a.lease(inA, false)
	id := a.chatID(inA)
	a.apply(ext.Op{ID: "q", Op: "post", Chat: id, Text: "Rain"})
	h.Posts()[0].OnSent([]int64{100}, false)
	img := []ext.Image{{Data: tinyPNG(), MimeType: "image/png"}}
	a.apply(ext.Op{ID: "e1", Op: "edit", Chat: id, Post: "q", Images: img})
	a.apply(ext.Op{ID: "e2", Op: "edit", Chat: id, Post: "q", Text: "Rain, 7 days"})
	if n := len(h.Posts()); n != 2 {
		t.Fatalf("%d posts before the first edit was sent", n)
	}
	h.Posts()[1].OnSent([]int64{200}, false) // sent anew
	if p := h.Posts(); len(p) != 3 || p[2].Edit != 200 {
		t.Fatalf("posts %+v", p)
	}
}

// pressConn is an addon whose tools answer "7 days", or fail with fail
// set, recording each call's params.
type pressConn struct {
	mu    sync.Mutex
	calls []map[string]any
	fail  bool
}

func (c *pressConn) call(_ context.Context, _ string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	data, _ := json.Marshal(params)
	var p map[string]any
	json.Unmarshal(data, &p)
	c.calls = append(c.calls, p)
	if c.fail {
		return nil, errors.New("down")
	}
	return json.RawMessage(`{"content":[{"type":"text","text":"7 days"}]}`), nil
}

func (c *pressConn) notify(context.Context, string, any) error { return nil }

// A press of a button under one of the addon's posts calls its onPress
// tool with the button's data and the post's ID, and the presser's
// context, whose ref is the post, and answers with the result's text. It
// needs no watch, and leases the chat, so the addon can edit the post
// however old; a post's buttons are known for 30 days. A press under
// another message isn't the addon's.
func TestPress(t *testing.T) {
	cfg := everything
	cfg.Names = true
	a, now, h := buttonsAddon(cfg)
	c := &pressConn{}
	a.conn = c
	a.lease(inA, false)
	id := a.chatID(inA)
	a.apply(ext.Op{ID: "q", Op: "post", Chat: id, Text: "Rain", Buttons: rainButtons, OnPress: "period"})
	h.Posts()[0].OnSent([]int64{101}, false)
	*now = now.Add(29 * 24 * time.Hour) // the lease is long gone
	alice := feature.Person{ID: "alice-id", Name: "Alice"}
	ctx := context.Background()
	answer, ok := a.press(ctx, feature.Press{Chat: inA, By: alice, Sent: 101, Data: "rain 7d"})
	if !ok || answer != "7 days" {
		t.Fatalf("answered %q, %v", answer, ok)
	}
	call := c.calls[0]
	cc := call["_meta"].(map[string]any)[ext.KeyContext].(map[string]any)
	if call["name"] != "period" || call["arguments"].(map[string]any)["press"] != "rain 7d" || call["arguments"].(map[string]any)["post"] != "q" ||
		cc["chat"] != id || cc["sender"] != a.userID(inA, "alice-id") || cc["name"] != "Alice" || cc["deadline"] == nil {
		t.Fatalf("call %+v", call)
	}
	if r := a.refs[cc["ref"].(string)]; !r.msg.Bot || r.msg.Timestamp != 101 || r.msg.Chat != inA {
		t.Fatalf("its ref %+v", r)
	}
	// The press leased the chat: an edit follows.
	if why := a.apply(ext.Op{ID: "e", Op: "edit", Chat: id, Post: "q", Text: "Rain, 7 days", Buttons: rainButtons, OnPress: "period"}); why != "" {
		t.Fatal(why)
	}
	for _, p := range []feature.Press{
		{Chat: inA, By: alice, Sent: 100, Data: "rain 7d"},                        // not the message with the buttons
		{Chat: inB, By: alice, Sent: 101, Data: "rain 7d"},                        // another chat
		{Chat: feature.Chat{GroupID: "-100999"}, By: alice, Sent: 101, Data: "x"}, // nor one the addon is in
		{Chat: inA, By: feature.Person{}, Sent: 101, Data: "rain 7d"},             // no one
		{Chat: inA, By: alice, Sent: 0, Data: "rain 7d"},                          // no message
		{Chat: inA, By: alice, Sent: 102, Data: "rain 7d"},                        // another
	} {
		if _, ok := a.press(ctx, p); ok {
			t.Errorf("%+v taken", p)
		}
	}
	c.fail = true
	if answer, ok := a.press(ctx, feature.Press{Chat: inA, By: alice, Sent: 101, Data: "rain 7d"}); !ok || answer != "Game couldn't do that just now." {
		t.Fatalf("a failed call answered %q, %v", answer, ok)
	}
	c.fail = false
	// A tool no longer for the bot alone is refused, without a call.
	a.only["period"] = false
	if answer, ok := a.press(ctx, feature.Press{Chat: inA, By: alice, Sent: 101, Data: "rain 7d"}); !ok || answer != "" || len(c.calls) != 2 {
		t.Fatalf("refused: %q, %v, %d calls", answer, ok, len(c.calls))
	}
	a.only["period"] = true
	*now = now.Add(31 * 24 * time.Hour)
	if _, ok := a.press(ctx, feature.Press{Chat: inA, By: alice, Sent: 101, Data: "rain 7d"}); ok {
		t.Fatal("a press under a post too old to be known")
	}
}

// Posts with buttons are known for 30 days, the newest 1,000; others for a
// day, the newest 256; both are saved, and restored.
func TestPostedKept(t *testing.T) {
	a, now, _ := buttonsAddon(everything)
	for i := range maxButtons + 10 {
		*now = now.Add(time.Second)
		a.rememberPosted(inA, []string{"b" + strings.Repeat("x", i%3) + string(rune('a'+i%26)) + time.Duration(i).String()}, []int64{int64(i + 1)}, "period")
	}
	for i := range maxPosted + 10 {
		*now = now.Add(time.Second)
		a.rememberPosted(inA, []string{"p" + time.Duration(i).String()}, []int64{int64(10000 + i)}, "")
	}
	plain, buttoned := 0, 0
	for _, p := range a.posted {
		if p.OnPress != "" {
			buttoned++
			if p.Sent <= 10 {
				t.Fatalf("an old post kept: %+v", p)
			}
		} else {
			plain++
		}
	}
	if plain != maxPosted || buttoned != maxButtons {
		t.Fatalf("%d plain, %d with buttons", plain, buttoned)
	}
	s := a.state()
	b, later, _ := buttonsAddon(everything)
	*later = now.Add(2 * 24 * time.Hour) // the plain ones gone
	b.restore(s)
	if len(b.posted) != maxButtons {
		t.Fatalf("restored %d", len(b.posted))
	}
	for k, p := range b.posted {
		if q := a.posted[k]; p != q {
			t.Fatalf("restored %+v, saved %+v", p, q)
		}
	}
}

// initialConn records initialize's params.
type initialConn struct{ params any }

func (c *initialConn) call(_ context.Context, method string, params any) (json.RawMessage, error) {
	if method == "initialize" {
		c.params = params
	}
	return json.RawMessage(`{}`), nil
}

func (c *initialConn) notify(context.Context, string, any) error { return nil }

// The bot offers an addon buttons only on a network that shows them.
func TestInitializeButtons(t *testing.T) {
	for _, c := range []struct {
		self botInfo
		want bool
	}{
		{botInfo{network: "telegram", addon: true, buttons: true}, true},
		{botInfo{network: "signal", addon: true}, false},
		{botInfo{network: "telegram", buttons: true}, false}, // not an addon
	} {
		conn := &initialConn{}
		if _, err := initialize(context.Background(), conn, c.self); err != nil {
			t.Fatal(err)
		}
		capability := conn.params.(map[string]any)["capabilities"].(map[string]any)["experimental"].(map[string]any)[ext.KeyAddon].(ext.Capability)
		if capability.Buttons != c.want {
			t.Errorf("%+v: buttons %v", c.self, capability.Buttons)
		}
	}
}

// An edit of a post merged with others, or sent in several messages, is
// refused; on a network without buttons (Signal), an edit in place keeps
// the post's first day, and only 10 are made.
func TestOutboxEditRefused(t *testing.T) {
	cfg := everything
	cfg.Rate = rateConfig{60, 60}
	a, now, h := buttonsAddon(cfg)
	a.lease(inA, false)
	id := a.chatID(inA)
	a.rememberPosted(inA, []string{"m1", "m2"}, []int64{100}, "")
	a.rememberPosted(inA, []string{"long"}, []int64{200, 201}, "")
	edit := func(n int, post string) string {
		return a.apply(ext.Op{ID: "e" + strconv.Itoa(n), Op: "edit", Chat: id, Post: post, Text: "Rain"})
	}
	if why := edit(1, "m1"); why != "edits of a post merged with others" {
		t.Fatalf("merged: %q", why)
	}
	if why := edit(2, "long"); why != "edits of a post sent in several messages" {
		t.Fatalf("several: %q", why)
	}

	a.caps = feature.Caps{EditLimit: 10, EditWindow: 24 * time.Hour} // Signal's
	a.rememberPosted(inA, []string{"s"}, []int64{300}, "")
	first := a.posted["s"].Expires
	for i := range a.caps.EditLimit {
		*now = now.Add(time.Minute)
		if why := edit(10+i, "s"); why != "" {
			t.Fatalf("edit %d: %q", i, why)
		}
		h.Posts()[len(h.Posts())-1].OnSent([]int64{300}, false)
	}
	if p := a.posted["s"]; !p.Expires.Equal(first) || p.Edits != a.caps.EditLimit {
		t.Fatalf("remembered %+v", p)
	}
	if why := edit(30, "s"); why != "edits past the network's limit" {
		t.Fatalf("an 11th edit: %q", why)
	}
	*now = first
	if why := edit(31, "s"); why != "edits of an unknown post" {
		t.Fatalf("past a day: %q", why)
	}
}

// A press under an old message in a forum topic, which names only the
// group, finds the post by its message, and acts in the topic.
func TestPressTopic(t *testing.T) {
	a, now, h := buttonsAddon(everything)
	c := &pressConn{}
	a.conn = c
	topic := feature.Chat{GroupID: telegram + "/7"}
	a.lease(topic, false)
	a.apply(ext.Op{ID: "q", Op: "post", Chat: a.chatID(topic), Text: "Rain", Buttons: rainButtons, OnPress: "period"})
	h.Posts()[0].OnSent([]int64{101}, false)
	if p := a.posted["q"]; p.Topic == nil || *p.Topic != topic {
		t.Fatalf("remembered %+v", p)
	}
	*now = now.Add(10 * 24 * time.Hour)
	alice := feature.Person{ID: "alice-id", Name: "Alice"}
	bare := feature.Chat{GroupID: telegram}
	if answer, ok := a.press(context.Background(), feature.Press{Chat: bare, By: alice, Sent: 101, Data: "rain 7d"}); !ok || answer != "7 days" {
		t.Fatalf("answered %q, %v", answer, ok)
	}
	cc := c.calls[0]["_meta"].(map[string]any)[ext.KeyContext].(map[string]any)
	if cc["chat"] != a.chatID(topic) {
		t.Fatalf("context %+v", cc)
	}
	if why := a.apply(ext.Op{ID: "e", Op: "edit", Chat: a.chatID(topic), Post: "q", Text: "Rain, 7 days"}); why != "" {
		t.Fatalf("the edit after: %q", why)
	}
	for _, p := range []feature.Press{
		{Chat: feature.Chat{GroupID: "-100999"}, By: alice, Sent: 101, Data: "x"}, // another group
		{Chat: bare, By: alice, Sent: 102, Data: "x"},                             // another message
	} {
		if _, ok := a.press(context.Background(), p); ok {
			t.Errorf("%+v taken", p)
		}
	}
}

// The addon's direct commands, as registered, are the ones a key may run.
func TestKeyCommands(t *testing.T) {
	a, _, h := buttonsAddon(everything)
	a.cmds = map[string]bool{}
	command(h, "game", "/Rain", chatCommand{Tool: "period", Direct: true}, nil, a)
	command(h, "game", "ask", chatCommand{Tool: "period"}, map[string]string{"period": "game_period"}, a)
	if !a.cmds["rain"] || a.cmds["ask"] || len(a.cmds) != 1 {
		t.Fatalf("commands %v", a.cmds)
	}
}

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	ext "github.com/robdevops/signalbot/addon"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/feature/featuretest"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

// pressConn answers each tools/call with result, after wait, and records
// the calls.
type pressConn struct {
	mu     sync.Mutex
	calls  []map[string]any
	result string
	wait   time.Duration
	err    error
}

func (c *pressConn) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	b, _ := json.Marshal(params)
	var p map[string]any
	json.Unmarshal(b, &p)
	c.mu.Lock()
	c.calls = append(c.calls, p)
	res, wait, err := c.result, c.wait, c.err
	c.mu.Unlock()
	select {
	case <-time.After(wait):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return json.RawMessage(res), err
}

func (c *pressConn) notify(context.Context, string, any) error { return nil }

func (c *pressConn) last() map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.calls) == 0 {
		return nil
	}
	return c.calls[len(c.calls)-1]
}

// buttonAddon is an addon in every chat whose tool "press" is for the bot
// alone and "both" for the model too, on a host whose network shows
// buttons if buttons.
func buttonAddon(c conn, buttons bool) (*addon, *featuretest.Host) {
	h := featuretest.New()
	h.Bot.Caps.Buttons = buttons
	a := newAddon("game", "Game", addonConfig{DMs: true, Chats: []string{"all"}, Names: true}, bytes.Repeat([]byte{7}, secretSize), c,
		map[string]bool{"cmd": true, "press": true, "both": true})
	a.only = map[string]bool{"cmd": true, "press": true, "both": false}
	a.host = h
	return a, h
}

// lobby is a direct result with one row of buttons calling onPress.
func lobby(onPress string, row ...ext.Button) string {
	r := ext.Text("GeoGuess starts in 60s: react to join.")
	r.Meta = &ext.ResultMeta{Buttons: [][]ext.Button{row}, OnPress: onPress}
	b, _ := json.Marshal(r)
	return string(b)
}

var startRow = []ext.Button{{Label: "▶️ Start", Data: "start:g1"}, {Label: "✖️ Cancel", Data: "cancel:g1"}, {Label: "⏱ +30s", Data: "wait:g1"}}

// A result's buttons are checked, and left off, with the answer still
// sent, if they break the rules or the chat's network shows none.
func TestAddonMarkup(t *testing.T) {
	ok := ext.Button{Label: "a", Data: "a"}
	rows := func(n, per int) [][]ext.Button {
		return slices.Repeat([][]ext.Button{slices.Repeat([]ext.Button{ok}, per)}, n)
	}
	for _, c := range []struct {
		name    string
		buttons [][]ext.Button
		onPress string
		why     string
	}{
		{"fine", [][]ext.Button{startRow}, "press", ""},
		{"five of five", rows(5, 5), "press", ""},
		{"a 32-character label", [][]ext.Button{{{Label: strings.Repeat("é", 32), Data: strings.Repeat("x", 64)}}}, "press", ""},
		{"no onPress", [][]ext.Button{{ok}}, "", "buttons without onPress"},
		{"no buttons", nil, "press", "buttons without onPress"},
		{"onPress for the model too", [][]ext.Button{{ok}}, "both", "isn't one of its tools for the bot alone"},
		{"onPress unknown", [][]ext.Button{{ok}}, "nope", "isn't one of its tools for the bot alone"},
		{"six rows", rows(6, 1), "press", "more than 5 rows"},
		{"six in a row", rows(1, 6), "press", "more than 5 rows"},
		{"an empty row", [][]ext.Button{{ok}, {}}, "press", "more than 5 rows"},
		{"an empty label", [][]ext.Button{{{Label: "", Data: "a"}}}, "press", "label or data"},
		{"a long label", [][]ext.Button{{{Label: strings.Repeat("x", 33), Data: "a"}}}, "press", "label or data"},
		{"a label with space", [][]ext.Button{{{Label: " a", Data: "a"}}}, "press", "label or data"},
		{"empty data", [][]ext.Button{{{Label: "a", Data: ""}}}, "press", "label or data"},
		{"long data", [][]ext.Button{{{Label: "a", Data: strings.Repeat("é", 33)}}}, "press", "label or data"},
	} {
		t.Run(c.name, func(t *testing.T) {
			a, _ := buttonAddon(nil, true)
			var r toolResult
			r.Meta.Buttons, r.Meta.OnPress = c.buttons, c.onPress
			var log logx.Buffer
			restore := logx.SetOutput(&log, &log, true)
			mk, tool := a.markup(logx.L{}, inA, r)
			restore()
			if c.why == "" {
				if tool != c.onPress || len(mk.Buttons) != len(c.buttons) || log.String() != "" {
					t.Errorf("markup %+v, %q; log %q", mk, tool, log.String())
				}
				return
			}
			if tool != "" || len(mk.Buttons) != 0 || !strings.Contains(log.String(), "MCP server game: buttons left off: ") || !strings.Contains(log.String(), c.why) {
				t.Errorf("markup %+v, %q; log %q", mk, tool, log.String())
			}
		})
	}
	a, _ := buttonAddon(nil, false)
	var r toolResult
	r.Meta.Buttons, r.Meta.OnPress = [][]ext.Button{startRow}, "press"
	if mk, tool := a.markup(logx.L{}, inA, r); len(mk.Buttons) != 0 || tool != "" {
		t.Errorf("buttons where the network has none: %+v", mk)
	}
}

// A direct answer gets its buttons where the network shows them, and is
// the same answer without them where it doesn't.
func TestDirectButtons(t *testing.T) {
	for _, buttons := range []bool{true, false} {
		c := &pressConn{result: lobby("press", startRow...)}
		a, _ := buttonAddon(c, buttons)
		act := a.direct(context.Background(), in(inA, "/geo"), "geo", chatCommand{Tool: "cmd", Direct: true}, "")
		if act.Answer != "GeoGuess starts in 60s: react to join." {
			t.Errorf("buttons %v: answer %q", buttons, act.Answer)
		}
		want := 0
		if buttons {
			want = 1
		}
		if len(act.Markup.Buttons) != want || want == 1 && (len(act.Markup.Buttons[0]) != 3 || act.Markup.Buttons[0][2] != (feature.Button{Label: "⏱ +30s", Data: "wait:g1"})) {
			t.Errorf("buttons %v: markup %+v", buttons, act.Markup)
		}
		act.OnSent([]int64{100, 101}, false)
		a.mu.Lock()
		kept := slices.Clone(a.presses)
		a.mu.Unlock()
		if buttons && (len(kept) != 1 || kept[0].Sent != 101 || kept[0].Tool != "press" || kept[0].Chat != inA) || !buttons && len(kept) != 0 {
			t.Errorf("buttons %v: kept %+v", buttons, kept)
		}
	}
}

// A press under an answer's buttons calls its onPress tool with the
// button's data and the presser's context, and shows the result's text.
func TestAddonPress(t *testing.T) {
	c := &pressConn{result: lobby("press", startRow...)}
	a, h := buttonAddon(c, true)
	h.OnPress(a.press)
	act := a.direct(context.Background(), in(inA, "/geo"), "geo", chatCommand{Tool: "cmd", Direct: true}, "")
	act.OnSent([]int64{100, 101}, false)

	c.mu.Lock()
	c.result = `{"content":[{"type":"text","text":"Starting!"}]}`
	c.mu.Unlock()
	answer, ok := h.Press(context.Background(), feature.Press{Chat: inA, By: feature.Person{ID: "bob-id", Name: "Bob"}, Sent: 101, Data: "start:g1"})
	if !ok || answer != "Starting!" {
		t.Fatalf("press: %q, %v", answer, ok)
	}
	call := c.last()
	if call["name"] != "press" {
		t.Errorf("called %v", call["name"])
	}
	if args, _ := json.Marshal(call["arguments"]); string(args) != `{"press":"start:g1"}` {
		t.Errorf("arguments %s", args)
	}
	var cc ext.Context
	b, _ := json.Marshal(call["_meta"].(map[string]any)[ext.KeyContext])
	json.Unmarshal(b, &cc)
	if cc.Sender != a.userID(inA, "bob-id") || cc.Chat != a.chatID(inA) || !strings.HasPrefix(cc.Ref, "r_") || !cc.Group || !cc.Addressed ||
		cc.Name != "Bob" || cc.Deadline == 0 {
		t.Errorf("context %+v", cc)
	}
	if !a.leases[a.chatID(inA)].Expires.After(a.now()) {
		t.Error("a press leased no chat")
	}

	// Under the answer's first message, another chat's, or one never sent:
	// not the addon's.
	for _, p := range []feature.Press{{Chat: inA, By: feature.Person{ID: "bob-id"}, Sent: 100, Data: "start:g1"},
		{Chat: inB, By: feature.Person{ID: "bob-id"}, Sent: 101, Data: "start:g1"},
		{Chat: inA, By: feature.Person{ID: "bob-id"}, Sent: 999, Data: "start:g1"}} {
		if answer, ok := a.press(context.Background(), p); ok || answer != "" {
			t.Errorf("press %+v: %q, %v", p, answer, ok)
		}
	}

	// A long answer is cut to a toast; a failure is told in words.
	c.mu.Lock()
	c.result = `{"content":[{"type":"text","text":"` + strings.Repeat("x", 500) + `"}]}`
	c.mu.Unlock()
	if answer, _ := a.press(context.Background(), feature.Press{Chat: inA, By: feature.Person{ID: "bob-id"}, Sent: 101, Data: "wait:g1"}); len([]rune(answer)) > maxToast {
		t.Errorf("toast of %d characters", len([]rune(answer)))
	}
	c.mu.Lock()
	c.result, c.err = "", errors.New("down")
	c.mu.Unlock()
	if answer, ok := a.press(context.Background(), feature.Press{Chat: inA, By: feature.Person{ID: "bob-id"}, Sent: 101, Data: "wait:g1"}); !ok || answer != "Game couldn't do that just now." {
		t.Errorf("failure: %q, %v", answer, ok)
	}

	// A tool no longer for the bot alone, such as after a restart, is
	// refused.
	c.mu.Lock()
	c.err, c.calls = nil, nil
	c.mu.Unlock()
	a.only["press"] = false
	if answer, ok := a.press(context.Background(), feature.Press{Chat: inA, By: feature.Person{ID: "bob-id"}, Sent: 101, Data: "start:g1"}); !ok || answer != "" || c.last() != nil {
		t.Errorf("refused tool: %q, %v, calls %v", answer, ok, c.calls)
	}
}

// A press that takes too long is given up on.
func TestAddonPressTimeout(t *testing.T) {
	defer func(d time.Duration) { pressTimeout = d }(pressTimeout)
	pressTimeout = 50 * time.Millisecond
	c := &pressConn{result: `{"content":[]}`, wait: time.Second}
	a, _ := buttonAddon(c, true)
	a.keepPress(pressPost{Chat: inA, Sent: 7, Tool: "press"})
	var log logx.Buffer
	t.Cleanup(logx.SetOutput(&log, &log, true))
	ctx := context.Background()
	if answer, ok := a.press(ctx, feature.Press{Chat: inA, By: feature.Person{ID: "bob-id"}, Sent: 7, Data: "start:g1"}); !ok || answer != "Game couldn't do that just now." {
		t.Errorf("press: %q, %v", answer, ok)
	}
	if l := log.String(); !strings.Contains(l, "MCP server game: a press called press: no answer in time") || strings.Contains(l, "bob") || strings.Contains(l, "start:g1") {
		t.Errorf("log %q", l)
	}
}

// Posts with buttons are kept for a day, the newest maxPressPosts.
func TestKeepPress(t *testing.T) {
	a, _ := buttonAddon(nil, true)
	now := time.Now()
	a.now = func() time.Time { return now }
	for i := range maxPressPosts + 5 {
		a.keepPress(pressPost{Chat: inA, Sent: int64(i + 1), Tool: "press"})
	}
	if len(a.presses) != maxPressPosts || a.presses[0].Sent != 6 {
		t.Errorf("%d kept, the oldest %d", len(a.presses), a.presses[0].Sent)
	}
	now = now.Add(pressLife)
	if _, ok := a.press(context.Background(), feature.Press{Chat: inA, By: feature.Person{ID: "bob-id"}, Sent: 10}); ok {
		t.Error("a press under a post a day old")
	}
}

// initialize offers buttons to an addon whose bot's network shows them,
// and to no plain server.
func TestInitializeButtons(t *testing.T) {
	for _, c := range []struct {
		self botInfo
		want bool
	}{
		{botInfo{version: "v", network: "telegram", addon: true, buttons: true}, true},
		{botInfo{version: "v", network: "telegram", buttons: true}, false},
		{botInfo{version: "v", network: "signal", addon: true}, false},
	} {
		ic := &initConn{}
		if _, err := initialize(context.Background(), ic, c.self); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(ic.params, `"buttons":true`); got != c.want {
			t.Errorf("%+v: %s", c.self, ic.params)
		}
	}
}

// End to end: an addon told it has buttons answers a command with them,
// and a press under the answer calls its onPress tool, whose text is the
// toast.
func TestAddonButtonsProcess(t *testing.T) {
	h, log := startAddon(t, `{"mcpServers":{"game":`+addonServer("addon", `{"chats":["all"],"names":true}`,
		`,"chat_commands":{"lobby":{"tool":"lobby","direct":true}}`)+`}}`,
		func(h *featuretest.Host) { h.Bot.Caps.Buttons = true; h.Bot.Network = "telegram" })
	act := h.Message(context.Background(), in(inA, "/lobby"))
	if act.Answer != "Starts in 60s" || len(act.Markup.Buttons) != 1 || act.Markup.Buttons[0][0] != (feature.Button{Label: "▶️ Start", Data: "start:g1"}) {
		t.Fatalf("lobby: %+v", act)
	}
	act.OnSent([]int64{42}, false)
	answer, ok := h.Press(context.Background(), feature.Press{Chat: inA, By: feature.Person{ID: "bob-id", Name: "Bob"}, Sent: 42, Data: "start:g1"})
	if !ok || answer != "start:g1 by @Bob" {
		t.Errorf("press: %q, %v; log %s", answer, ok, log.String())
	}
	if _, ok := h.Press(context.Background(), feature.Press{Chat: inA, By: feature.Person{ID: "bob-id"}, Sent: 41, Data: "start:g1"}); ok {
		t.Error("a press under another message taken")
	}
	if l := log.String(); !strings.Contains(l, "MCP server game: a press called pressed: ok in ") || strings.Contains(l, "Bob") {
		t.Errorf("log %q", l)
	}
}

// lobbyWatch is lobby whose result also starts the watch called id.
func lobbyWatch(id string, row ...ext.Button) string {
	r := ext.Text("GeoGuess starts in 60s: react to join.")
	r.Meta = &ext.ResultMeta{Buttons: [][]ext.Button{row}, OnPress: "press",
		Watch: &ext.Watch{ID: id, Pattern: `\bgeo \d+`, Until: time.Now().Add(time.Hour).Format(time.RFC3339), Tool: "cmd"}}
	b, _ := json.Marshal(r)
	return string(b)
}

// A buttons op replaces the buttons under the addon's direct answer whose
// result started its watch, by an edit with the answer's same text, and
// presses still call its onPress; an unknown watch, or an answer sent
// where the network shows no buttons, is done with nothing posted, and
// buttons that break the rules drop the op.
func TestButtonsOp(t *testing.T) {
	c := &pressConn{result: lobbyWatch("lobby1", startRow[1:]...)}
	a, h := buttonAddon(c, true)
	a.cfg.Watch = true
	act := a.direct(context.Background(), in(inA, "/geo"), "geo", chatCommand{Tool: "cmd", Direct: true}, "")
	act.OnSent([]int64{101}, false)
	chat := a.chatID(inA)

	start := [][]ext.Button{startRow}
	if why := a.apply(ext.Op{ID: "1", Op: "buttons", Chat: chat, Watch: "lobby1", Buttons: start}); why != "" {
		t.Fatalf("dropped: %s", why)
	}
	posts := h.Posts()
	if len(posts) != 1 || posts[0].Edit != 101 || posts[0].Chat != inA || posts[0].Text != act.Answer ||
		len(posts[0].Markup.Buttons) != 1 || len(posts[0].Markup.Buttons[0]) != 3 || posts[0].Markup.Buttons[0][0] != (feature.Button{Label: "▶️ Start", Data: "start:g1"}) {
		t.Fatalf("posts %+v", posts)
	}
	posts[0].OnSent([]int64{101}, false)
	a.mu.Lock()
	kept := slices.Clone(a.presses)
	a.mu.Unlock()
	if len(kept) != 1 || kept[0].Sent != 101 || kept[0].Tool != "press" || kept[0].Edits != 1 {
		t.Errorf("kept %+v", kept)
	}
	if again := a.apply(ext.Op{ID: "1", Op: "buttons", Chat: chat, Watch: "lobby1", Buttons: start}); again != "" || len(h.Posts()) != 1 {
		t.Errorf("an op done twice: %q, %d posts", again, len(h.Posts()))
	}

	// Unknown: another watch, another chat's, an empty watch.
	for i, o := range []ext.Op{{Watch: "lobby2", Chat: chat}, {Watch: "lobby1", Chat: a.chatID(inB)}, {Chat: chat}} {
		o.ID, o.Op, o.Buttons = "u"+string(rune('0'+i)), "buttons", start
		if why := a.apply(o); why != "" || len(h.Posts()) != 1 {
			t.Errorf("%+v: %q, %d posts", o, why, len(h.Posts()))
		}
	}
	// Bad buttons.
	for i, b := range [][][]ext.Button{nil, {{}}, slices.Repeat(start, 6), {{{Label: strings.Repeat("x", 33), Data: "a"}}}, {{{Label: "a", Data: strings.Repeat("x", 65)}}}} {
		if why := a.apply(ext.Op{ID: "b" + string(rune('0'+i)), Op: "buttons", Chat: chat, Watch: "lobby1", Buttons: b}); !strings.HasPrefix(why, "buttons ops with ") || len(h.Posts()) != 1 {
			t.Errorf("buttons %v: %q, %d posts", b, why, len(h.Posts()))
		}
	}

	// Past the network's edit limits: left as it is.
	h.Bot.Caps.EditLimit = 1
	if why := a.apply(ext.Op{ID: "l", Op: "buttons", Chat: chat, Watch: "lobby1", Buttons: start}); why != "" || len(h.Posts()) != 1 {
		t.Errorf("past the limit: %q, %d posts", why, len(h.Posts()))
	}
	h.Bot.Caps.EditLimit = 0

	// With text: the answer's text replaced, placeholders filled in, and
	// kept by a later buttons op without text; too long, clamped.
	if why := a.apply(ext.Op{ID: "t1", Op: "buttons", Chat: chat, Watch: "lobby1", Buttons: start, Text: " 5 rounds, by {" + a.userID(inA, "x") + "} "}); why != "" {
		t.Fatalf("with text: dropped: %s", why)
	}
	posts = h.Posts()
	if len(posts) != 2 || posts[1].Edit != 101 || posts[1].Text != "5 rounds, by someone" {
		t.Fatalf("with text: posts %+v", posts)
	}
	posts[1].OnSent([]int64{101}, false)
	if why := a.apply(ext.Op{ID: "t2", Op: "buttons", Chat: chat, Watch: "lobby1", Buttons: start}); why != "" {
		t.Fatalf("after text: dropped: %s", why)
	}
	posts = h.Posts()
	if len(posts) != 3 || posts[2].Text != "5 rounds, by someone" {
		t.Fatalf("after text: posts %+v", posts)
	}
	posts[2].OnSent([]int64{101}, false)
	if why := a.apply(ext.Op{ID: "t3", Op: "buttons", Chat: chat, Watch: "lobby1", Buttons: start, Text: strings.Repeat("x", maxAnswer+50)}); why != "" {
		t.Fatalf("long text: dropped: %s", why)
	}
	posts = h.Posts()
	if len(posts) != 4 || utf8.RuneCountInString(posts[3].Text) != maxAnswer+1 || !strings.HasSuffix(posts[3].Text, "x…") {
		t.Fatalf("long text: %d posts, %d runes", len(posts), utf8.RuneCountInString(posts[len(posts)-1].Text))
	}
	posts[3].OnSent([]int64{101}, false)

	// An answer with pictures: the op is dropped.
	a.mu.Lock()
	a.presses[0].Pics = true
	a.mu.Unlock()
	if why := a.apply(ext.Op{ID: "p", Op: "buttons", Chat: chat, Watch: "lobby1", Buttons: start}); why != "buttons ops for an answer with pictures" || len(h.Posts()) != 4 {
		t.Errorf("pictures: %q, %d posts", why, len(h.Posts()))
	}

	// Where the network shows no buttons (Signal), the answer was never
	// remembered: nothing to do.
	c2 := &pressConn{result: lobbyWatch("lobby1", startRow[1:]...)}
	s, sh := buttonAddon(c2, false)
	s.cfg.Watch = true
	sact := s.direct(context.Background(), in(inA, "/geo"), "geo", chatCommand{Tool: "cmd", Direct: true}, "")
	sact.OnSent([]int64{101}, false)
	if why := s.apply(ext.Op{ID: "1", Op: "buttons", Chat: s.chatID(inA), Watch: "lobby1", Buttons: start}); why != "" || len(sh.Posts()) != 0 {
		t.Errorf("signal: %q, posts %+v", why, sh.Posts())
	}
}

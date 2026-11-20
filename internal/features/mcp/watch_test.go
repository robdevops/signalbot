package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/feature/featuretest"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

// --- the fake addon's game and outbox -------------------------------------

// fakeOutbox is the fake addon's outbox, and its game's tools: round
// answers with a picture and the watch its args spell out in JSON; guess
// records what it was sent, in GAME_LOG, and asks for a 📍 unless the text
// says nopin; queue adds the ops in its args ({"ops": […], "notify": n}) and
// says so n times (once if unset); epoch makes every earlier cursor
// unknown, as a restart would; crash exits. A read returns at most three
// ops a page, from the cursor if it knows it, else from the start, and logs
// how many reads were in flight, then how many ops it returned.
type fakeOutbox struct {
	notify func()

	mu      sync.Mutex
	nonce   string
	ops     []json.RawMessage
	reading int
}

func (b *fakeOutbox) call(name string, args map[string]string, meta json.RawMessage) string {
	text := func(s string) string { return `"result":{"content":[{"type":"text","text":"` + s + `"}]}` }
	switch name {
	case "round":
		return `"result":{"content":[{"type":"text","text":"Round on"},{"type":"image","mimeType":"image/png","data":"` + tinyPNG() + `"}],` +
			`"_meta":{"io.github.robdevops.signalbot/watch":` + args["args"] + `}}`
	case "guess":
		var m map[string]callContext
		json.Unmarshal(meta, &m)
		line, _ := json.Marshal(guessed{args["text"], m[vendor+"context"]})
		fakeLog("guess " + string(line))
		if strings.Contains(args["text"], "nopin") {
			return text("Recorded")
		}
		return `"result":{"content":[{"type":"text","text":"Recorded"}],"_meta":{"io.github.robdevops.signalbot/react":"📍"}}`
	case "queue":
		var q struct {
			Ops    []json.RawMessage `json:"ops"`
			Notify *int              `json:"notify"`
		}
		json.Unmarshal([]byte(args["args"]), &q)
		b.mu.Lock()
		b.ops = append(b.ops, q.Ops...)
		b.mu.Unlock()
		n := 1
		if q.Notify != nil {
			n = *q.Notify
		}
		for range n {
			b.notify()
		}
		return text("queued")
	case "epoch":
		b.mu.Lock()
		b.nonce += "a"
		b.mu.Unlock()
		return text("new epoch")
	case "crash":
		os.Exit(3)
	}
	return text("?")
}

func (b *fakeOutbox) read(uri string) string {
	b.mu.Lock()
	b.reading++
	fakeLog("reading " + strconv.Itoa(b.reading))
	b.mu.Unlock()
	time.Sleep(20 * time.Millisecond) // so a read that raced it would show
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reading--
	from := 0
	if n, ok := strings.CutPrefix(strings.TrimPrefix(uri, outboxURI+"/"), b.nonce+"."); ok {
		if i, err := strconv.Atoi(n); err == nil && i <= len(b.ops) {
			from = i
		}
	}
	to := min(from+3, len(b.ops))
	p, _ := json.Marshal(map[string]any{"cursor": fmt.Sprintf("%s.%d", b.nonce, to), "more": to < len(b.ops), "ops": b.ops[from:to]})
	res, _ := json.Marshal(map[string]any{"contents": []any{map[string]string{"uri": uri, "mimeType": "application/json", "text": string(p)}}})
	fakeLog("read " + strconv.Itoa(to-from))
	return `"result":` + string(res)
}

// guessed is what the fake addon's guess tool was sent.
type guessed struct {
	Text    string
	Context callContext
}

var fakeLogMu sync.Mutex

// fakeLog appends line to the file GAME_LOG names, if set: the fake addon
// gets it, as its name starts with GAME_.
func fakeLog(line string) {
	path := os.Getenv("GAME_LOG")
	if path == "" {
		return
	}
	fakeLogMu.Lock()
	defer fakeLogMu.Unlock()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintln(f, line)
}

// gameLog is the lines the fake addon logged that start with prefix,
// without it.
func gameLog(t *testing.T, prefix string) []string {
	t.Helper()
	f, err := os.Open(os.Getenv("GAME_LOG"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), prefix); ok {
			out = append(out, rest)
		}
	}
	return out
}

// guesses is what the fake addon's guess tool was sent, in order.
func guesses(t *testing.T) []guessed {
	var out []guessed
	for _, l := range gameLog(t, "guess ") {
		var g guessed
		json.Unmarshal([]byte(l), &g)
		out = append(out, g)
	}
	return out
}

// eventually waits up to 10s for ok.
func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); !ok(); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// --- helpers ------------------------------------------------------------------

// gameCommands are /round, /ctx, /queue, /epoch and /crash, each calling its
// tool directly.
const gameCommands = `,"chat_commands":{"round":{"tool":"round","direct":true},"ctx":{"tool":"context","direct":true},` +
	`"queue":{"tool":"queue","direct":true},"epoch":{"tool":"epoch","direct":true},"crash":{"tool":"crash","direct":true}}`

// gameConfig is MCP_CONFIG for the fake addon "game", allowed everything in
// group A and DMs, with more settings in its addon block.
func gameConfig(more string) string {
	return `{"mcpServers":{"game":` + addonServer("addon", `{"chats":["`+groupA+`"],"dms":true,"post":true,"react":true,"watch":true`+more+`}`, gameCommands) + `}}`
}

// startGame starts the fake addon "game" with config in the state
// directory dir, with GAME_LOG in a fresh file.
func startGame(t *testing.T, config, dir string) (*featuretest.Host, *logx.Buffer) {
	t.Helper()
	if os.Getenv("GAME_LOG") == "" {
		t.Setenv("GAME_LOG", filepath.Join(t.TempDir(), "game.log"))
	}
	h, log, err := register(t, config, func(h *featuretest.Host) { h.Settings["STATE_DIRECTORY"] = dir })
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the first read", func() bool { return len(gameLog(t, "read ")) > 0 })
	return h, log
}

// said is a message from who ("alice-id" or "bob-id") in chat, sent at ts.
func said(chat feature.Chat, who string, ts int64, text string) feature.Message {
	name := map[string]string{"alice-id": "Alice", "bob-id": "Bob"}[who]
	return feature.Message{Chat: chat, Sender: who, Name: name, Text: text, Timestamp: ts}
}

// to is m addressed to the bot.
func to(m feature.Message) feature.Message { m.Addressed = true; return m }

// replyTo is m addressed to the bot, quoting its message sent at sent.
func replyTo(m feature.Message, sent int64) feature.Message {
	m.Addressed, m.Quote = true, &feature.Quote{Bot: true, Sent: sent}
	return m
}

// edit is m edited at at to say text.
func edit(m feature.Message, at int64, text string) feature.Message {
	m.Edited, m.Text = at, text
	return m
}

// spec is a watch called id, for "guess <number>", lasting d, with guard.
func spec(id string, d time.Duration, guard string) string {
	b, _ := json.Marshal(watchSpec{ID: id, Pattern: `\bguess \d+`, Until: time.Now().Add(d).Format(time.RFC3339), Tool: "guess", Guard: guard})
	return string(b)
}

// round starts a round, watch spec, by Alice in chat at ts, and says its
// picture went out at 5000.
func round(t *testing.T, h *featuretest.Host, chat feature.Chat, ts int64, spec string) feature.Action {
	t.Helper()
	act := h.Message(context.Background(), to(said(chat, "alice-id", ts, "/round "+spec)))
	if act.Answer != "Round on" || len(act.Pictures) != 1 {
		t.Fatalf("round: %+v", act)
	}
	if act.OnSent != nil {
		act.OnSent([]int64{5000})
	}
	return act
}

// chatID is the fake addon's pseudonym for chat, from /ctx there.
func chatID(t *testing.T, h *featuretest.Host, chat feature.Chat) callContext {
	t.Helper()
	var meta map[string]callContext
	if err := json.Unmarshal([]byte(h.Message(context.Background(), in(chat, "/ctx")).Answer), &meta); err != nil {
		t.Fatal(err)
	}
	return meta[vendor+"context"]
}

// queue has the fake addon queue ops, telling the bot notify times.
func queue(t *testing.T, h *featuretest.Host, notify int, ops ...map[string]any) {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"ops": ops, "notify": notify})
	if got := h.Message(context.Background(), in(inA, "/queue "+string(b))); got.Answer != "queued" {
		t.Fatalf("queue: %+v", got)
	}
}

// --- watches, through the fake addon --------------------------------------

// A watch takes the matching messages in its chat sent after it, addressed
// or not, and their edits: each is Done, and goes to the watch's tool,
// which has the bot react. A reply to its picture that doesn't match gets
// the guard; nothing else is touched, and no text reaches the log.
func TestWatch(t *testing.T) {
	h, log := startGame(t, gameConfig(""), t.TempDir())
	ctx := context.Background()
	act := round(t, h, inA, 1000, spec("r1", time.Hour, "No hints during a round"))
	if act.OnSent == nil {
		t.Fatal("no OnSent, so no guard")
	}
	bobs := said(inA, "bob-id", 1001, "guess 42")
	for _, m := range []feature.Message{
		bobs, // unaddressed: on the reader goroutine
		replyTo(said(inA, "alice-id", 1002, "my guess 7"), 5000),
		to(said(inA, "alice-id", 1003, "@bot guess 8")),
		edit(bobs, 1004, "guess 43"),       // a claimed message's edit
		edit(bobs, 1005, "oops, not that"), // still claimed, though it no longer matches
	} {
		if got := h.Message(ctx, m); !got.Done || got.Answer != "" || len(got.Pictures) != 0 {
			t.Fatalf("%q: %+v", m.Text, got)
		}
	}
	for _, m := range []feature.Message{
		edit(said(inA, "bob-id", 999, "hello"), 1006, "guess 2"), // an older message's edit
		said(inA, "bob-id", 1007, "lol"),
		to(said(inA, "bob-id", 1008, "where is it?")),
		said(inB, "bob-id", 1009, "guess 3"),  // another group
		said(inDM, "bob-id", 1010, "guess 4"), // a DM, with no round
	} {
		if got := h.Message(ctx, m); !got.IsZero() {
			t.Fatalf("%q: %+v", m.Text, got)
		}
	}
	for _, text := range []string{"@bot where is this?", ""} { // with no text, such as a bare mention
		if got := h.Message(ctx, replyTo(said(inA, "bob-id", 1011, text), 5000)); got.Answer != "No hints during a round" || got.Done {
			t.Fatalf("guard for %q: %+v", text, got)
		}
	}

	eventually(t, "the guesses", func() bool { return len(guesses(t)) == 5 })
	g := guesses(t)
	var texts []string
	for _, x := range g {
		texts = append(texts, x.Text)
	}
	if want := []string{"guess 42", "my guess 7", "@bot guess 8", "guess 43", "oops, not that"}; !slices.Equal(texts, want) {
		t.Fatalf("the addon got %q", texts)
	}
	first := g[0].Context
	if first.Edited != 0 || first.Addressed || !first.Group || g[1].Context.Sender == first.Sender || !g[1].Context.Addressed {
		t.Fatalf("contexts %+v", g)
	}
	for _, e := range g[3:] {
		if e.Context.Sent != first.Sent || e.Context.Edited == 0 || e.Context.Sender != first.Sender || e.Context.Chat != first.Chat {
			t.Fatalf("an edit's context %+v, the original's %+v", e.Context, first)
		}
	}
	eventually(t, "the pins", func() bool { return len(h.Reactions()) == 5 })
	for i, r := range h.Reactions() {
		if r.Emoji != "📍" || r.Chat != inA || r.Message.Text != "" || r.Message.Timestamp < 1001 || r.Message.Sender == "" {
			t.Fatalf("reaction %d: %+v", i, r)
		}
	}
	l := log.String()
	if !regexp.MustCompile(`<6>MCP server game: watching a chat for (59m59s|1h0m0s)\n`).MatchString(l) ||
		!regexp.MustCompile(`<6>MCP server game: a watch called guess: ok in \d+ms, \d+ bytes\n`).MatchString(l) {
		t.Fatalf("log %q", l)
	}
	for _, s := range []string{"guess 4", "guess 1", "oops", "hints", "lol"} {
		if strings.Contains(l, s) {
			t.Fatalf("%q reached the log: %q", s, l)
		}
	}
}

// A watch must be allowed, and its pattern specific; it lasts at most
// watch_max.
func TestWatchRefused(t *testing.T) {
	h, log := startGame(t, gameConfig(`,"watch_max":"2s"`), t.TempDir())
	ctx := context.Background()
	for _, s := range []string{
		`{"id":"a","pattern":".*","until":"` + time.Now().Add(time.Hour).Format(time.RFC3339) + `","tool":"guess"}`,
		`{"id":"b","pattern":"(?i)\\b(see|you)\\b","until":"` + time.Now().Add(time.Hour).Format(time.RFC3339) + `","tool":"guess"}`,
		`{"id":"c","pattern":"guess (?=\\d)","until":"` + time.Now().Add(time.Hour).Format(time.RFC3339) + `","tool":"guess"}`,
		`{"id":"d","pattern":"guess \\d+","until":"` + time.Now().Add(time.Hour).Format(time.RFC3339) + `","tool":"plain"}`,
		`{"id":"e","pattern":"guess \\d+","until":"yesterday","tool":"guess"}`,
		`{"id":"f","pattern":"guess \\d+` + strings.Repeat("x", 1024) + `","until":"` + time.Now().Add(time.Hour).Format(time.RFC3339) + `","tool":"guess"}`,
	} {
		round(t, h, inA, 1000, s)
	}
	if got := h.Message(ctx, said(inA, "bob-id", 1001, "see you at guess 5")); !got.IsZero() {
		t.Fatalf("a refused watch took a message: %+v", got)
	}
	for _, want := range []string{"its pattern matches ordinary chat", "its pattern isn't RE2", "its tool isn't one the bot may call",
		"its until isn't an RFC 3339 time", "its pattern is over 1 KB"} {
		if !strings.Contains(log.String(), "<4>MCP server game: a watch refused: "+want+"\n") {
			t.Errorf("log %q, want %q", log.String(), want)
		}
	}

	// Asked for an hour, it lasts watch_max.
	round(t, h, inA, 2000, spec("r1", time.Hour, ""))
	if got := h.Message(ctx, said(inA, "bob-id", 2001, "guess 5")); !got.Done {
		t.Fatalf("a live watch: %+v", got)
	}
	time.Sleep(2100 * time.Millisecond)
	if got := h.Message(ctx, said(inA, "bob-id", 2002, "guess 6")); !got.IsZero() {
		t.Fatalf("an ended watch: %+v", got)
	}

	// Without the watch permission, none.
	h2, log2 := startGame(t, `{"mcpServers":{"game":`+addonServer("addon", `{"dms":true}`, gameCommands)+`}}`, t.TempDir())
	round(t, h2, inDM, 1000, spec("r1", time.Hour, ""))
	if got := h2.Message(ctx, said(inDM, "alice-id", 1001, "guess 5")); !got.IsZero() || !strings.Contains(log2.String(), "a watch refused: the addon block doesn't allow watch") {
		t.Fatalf("%+v, log %q", got, log2.String())
	}
}

// gatedStore is a Store whose saves wait while hold is locked.
type gatedStore struct {
	feature.Store
	hold  sync.Mutex
	saves chan struct{}
}

func (s *gatedStore) Save(v any) error {
	s.hold.Lock()
	defer s.hold.Unlock()
	err := s.Store.Save(v)
	select {
	case s.saves <- struct{}{}:
	default:
	}
	return err
}

// gatedHost is a featuretest.Host whose "mcp" store is a gatedStore.
type gatedHost struct {
	*featuretest.Host
	store *gatedStore
}

func (g gatedHost) Store(name string) (feature.Store, error) {
	s, err := g.Host.Store(name)
	if err != nil {
		return nil, err
	}
	g.store.Store = s
	return g.store, nil
}

// A match changes only memory: it never waits for a save, which happens on
// the saver goroutine, even while the disk is stuck.
func TestWatchNeverSavesOnTheReader(t *testing.T) {
	t.Setenv("GAME_LOG", filepath.Join(t.TempDir(), "game.log"))
	var log logx.Buffer
	t.Cleanup(logx.SetOutput(&log, &log, true))
	dir := t.TempDir()
	host := featuretest.New()
	host.Bot.Model, host.Bot.CallsTools = "x/test-model", true
	host.Settings["MCP_CONFIG"], host.Settings["STATE_DIRECTORY"] = writeConfig(t, gameConfig("")), dir
	t.Cleanup(host.Stop)
	g := gatedHost{host, &gatedStore{saves: make(chan struct{}, 1)}}
	if err := Register(g); err != nil {
		t.Fatal(err)
	}
	round(t, host, inA, 1000, spec("r1", time.Hour, ""))
	eventually(t, "a save", func() bool {
		select {
		case <-g.store.saves:
			return true
		default:
			return false
		}
	})

	g.store.hold.Lock() // the disk is stuck
	done := make(chan feature.Action)
	go func() { done <- host.Message(context.Background(), said(inA, "bob-id", 1001, "guess 42")) }()
	select {
	case got := <-done:
		if !got.Done {
			t.Fatalf("%+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a match waited for a save")
	}
	g.store.hold.Unlock()
	eventually(t, "the claim saved", func() bool {
		data, _ := os.ReadFile(filepath.Join(dir, "mcp.json"))
		var s saved
		json.Unmarshal(data, &s)
		return len(s.Addons["game"].Claims) == 1
	})
}

// Watches, claims and leases outlive a restart of the bot, and of the
// addon, which the bot subscribes to again.
func TestWatchRestarts(t *testing.T) {
	defer func(d time.Duration) { restartDelay = d }(restartDelay)
	restartDelay = 10 * time.Millisecond
	dir := t.TempDir()
	h, _ := startGame(t, gameConfig(""), dir)
	ctx := context.Background()
	round(t, h, inA, 1000, spec("r1", time.Hour, "No hints"))
	bobs := said(inA, "bob-id", 1001, "guess 42")
	if !h.Message(ctx, bobs).Done {
		t.Fatal("not taken")
	}
	chat := chatID(t, h, inA).Chat
	h.Stop()

	h, log := startGame(t, gameConfig(""), dir)
	for _, m := range []feature.Message{said(inA, "alice-id", 1002, "guess 5"), edit(bobs, 1003, "never mind")} {
		if got := h.Message(ctx, m); !got.Done {
			t.Fatalf("after a restart, %q: %+v", m.Text, got)
		}
	}
	if got := h.Message(ctx, replyTo(said(inA, "bob-id", 1004, "@bot where?"), 5000)); got.Answer != "No hints" {
		t.Fatalf("after a restart, the guard: %+v", got)
	}
	queue(t, h, 1, map[string]any{"id": "1", "op": "post", "chat": chat, "text": "after a restart"})
	eventually(t, "the post", func() bool { return len(h.Posts()) == 1 })

	// The addon restarts: the bot subscribes again, and its watches stand.
	h.Message(ctx, in(inA, "/crash"))
	eventually(t, "a second subscription", func() bool { return len(gameLog(t, "subscribed")) == 3 })
	if got := h.Message(ctx, said(inA, "alice-id", 1005, "guess 6")); !got.Done {
		t.Fatalf("after the addon's restart: %+v", got)
	}
	eventually(t, "the guesses", func() bool { return len(guesses(t)) >= 3 })
	if !strings.Contains(log.String(), "<4>MCP server game exited; restarting in") {
		t.Fatalf("log %q", log.String())
	}
}

// --- the outbox, through the fake addon ------------------------------------

// Posts go to a chat the addon has a lease in, one waiting at a time, the
// rest held and merged; one dropped before it was sent is posted again; an
// op read again is applied once, even from the start of an unknown cursor;
// reactions go to known messages.
func TestOutbox(t *testing.T) {
	d := retryDelay
	t.Cleanup(func() { retryDelay = d }) // after the addon stops
	retryDelay = 50 * time.Millisecond
	h, log := startGame(t, gameConfig(""), t.TempDir())
	ctx := context.Background()
	c := chatID(t, h, inA) // a lease, and a ref
	post := func(id, text string) map[string]any {
		return map[string]any{"id": id, "op": "post", "chat": c.Chat, "text": text}
	}
	queue(t, h, 1, post("1", "hello {"+c.Sender+"}"))
	eventually(t, "the post", func() bool { return len(h.Posts()) == 1 })
	if p := h.Posts()[0]; p.Chat != inA || p.Label != "Game" || p.Text != "hello @Alice" || p.Done == nil {
		t.Fatalf("post %+v", p)
	}

	// Held while the first waits, and merged.
	queue(t, h, 1, post("2", "two"), post("3", "three"))
	time.Sleep(100 * time.Millisecond)
	if n := len(h.Posts()); n != 1 {
		t.Fatalf("%d posts while one waits", n)
	}
	h.Posts()[0].Done([]int64{1}, false)
	eventually(t, "the held posts", func() bool { return len(h.Posts()) == 2 })
	if p := h.Posts()[1]; p.Text != "two\n\nthree" {
		t.Fatalf("held %q", p.Text)
	}

	// Dropped before it was sent, as while the bot stops: posted again,
	// one by one, as nothing waits.
	h.Posts()[1].Done(nil, true)
	eventually(t, "the post again", func() bool { return len(h.Posts()) == 3 })
	h.Posts()[2].Done([]int64{2}, false)
	eventually(t, "the next post again", func() bool { return len(h.Posts()) == 4 })
	h.Posts()[3].Done([]int64{3}, false)
	if p := h.Posts(); p[2].Text != "two" || p[3].Text != "three" {
		t.Fatalf("again %q, %q", p[2].Text, p[3].Text)
	}

	// Read again, from the start, after the addon's cursor is unknown: only
	// the new op is applied.
	h.Message(ctx, in(inA, "/epoch"))
	queue(t, h, 1, post("3", "three again"), post("4", "four"))
	eventually(t, "the new post", func() bool { return len(h.Posts()) == 5 })
	time.Sleep(100 * time.Millisecond)
	if p := h.Posts(); len(p) != 5 || p[4].Text != "four" {
		t.Fatalf("posts %+v", p)
	}
	h.Posts()[4].Done(nil, false)

	// A reaction, to a message the addon has a ref for; ops it may not do,
	// or for a chat it has no lease in, are dropped and counted.
	queue(t, h, 1, map[string]any{"id": "5", "op": "react", "ref": c.Ref, "emoji": "👀"},
		map[string]any{"id": "6", "op": "react", "ref": "r_unknown", "emoji": "👀"},
		post("", "no id"), map[string]any{"id": "7", "op": "post", "chat": "c_elsewhere", "text": "x"},
		map[string]any{"id": "8", "op": "dance"})
	eventually(t, "the reaction", func() bool { return len(h.Reactions()) == 1 })
	if r := h.Reactions()[0]; r.Emoji != "👀" || r.Chat != inA || r.Message.Sender != "alice-id" || r.Message.Timestamp != 1700000000000 {
		t.Fatalf("reaction %+v", r)
	}
	eventually(t, "the drops", func() bool {
		return strings.Contains(log.String(), "<4>addon game: dropped 4 ops: 1 for a chat without a lease, 1 of an unknown kind, 1 reactions to an unknown message, 1 without an id\n")
	})
	if len(h.Posts()) != 5 || strings.Contains(log.String(), "hello") {
		t.Fatalf("posts %d, log %q", len(h.Posts()), log.String())
	}
}

// A post goes as a merged post only while their pictures fit in one; the
// cursor moves past a post only once it is sent; one never sent is given up
// after maxPostTries.
func TestOutboxDelivery(t *testing.T) {
	defer func(d time.Duration) { retryDelay = d }(retryDelay)
	retryDelay = time.Hour // retried only when the test says
	var log logx.Buffer
	t.Cleanup(logx.SetOutput(&log, &log, true))
	cfg := everything
	cfg.Rate = rateConfig{600, 100}
	a, now := unitAddon(cfg)
	h := featuretest.New()
	a.host = h
	a.lease(inA)
	id := a.chatID(inA)
	pic := []postImage{{Data: tinyPNG(), MimeType: "image/png"}}
	postOp := func(n string, images []postImage) op {
		return op{ID: n, Op: "post", Chat: id, Text: "post " + n, Images: images}
	}
	for _, o := range []op{postOp("1", nil), postOp("2", pic), postOp("3", pic), postOp("4", pic), postOp("5", nil)} {
		if why := a.apply(o); why != "" {
			t.Fatalf("%s: %s", o.ID, why)
		}
	}
	a.next = "c.5"
	a.settle()
	if a.cursor != "" {
		t.Fatalf("the cursor moved to %q before the posts were sent", a.cursor)
	}
	var texts []string
	for i := 0; i < len(h.Posts()); i++ {
		texts = append(texts, h.Posts()[i].Text)
		h.Posts()[i].Done([]int64{int64(i)}, false)
	}
	if want := []string{"post 1", "post 2\n\npost 3", "post 4\n\npost 5"}; !slices.Equal(texts, want) {
		t.Fatalf("posts %q", texts)
	}
	if a.cursor != "c.5" || len(a.applied) != 5 {
		t.Fatalf("cursor %q, applied %q", a.cursor, a.applied)
	}

	// Never sent: posted again on each read once due, then given up.
	for try := range maxPostTries {
		a.next = "c.6"
		if why := a.apply(postOp("6", nil)); why != "" {
			t.Fatal(why)
		}
		p := h.Posts()[len(h.Posts())-1]
		if p.Text != "post 6" {
			t.Fatalf("try %d: %q", try, p.Text)
		}
		p.Done(nil, true)
		if a.apply(postOp("6", nil)); len(h.Posts()) != 4+try {
			t.Fatalf("try %d: posted again before it was due", try)
		}
		*now = now.Add(maxRetryDelay)
	}
	if !a.done["6"] || a.cursor != "c.5" || !strings.Contains(log.String(), "<4>addon game: 1 posts dropped: not sent after 6 tries\n") {
		t.Fatalf("cursor %q, log %q", a.cursor, log.String())
	}
}

// One reader per addon: a burst of news never makes two reads at once, a
// read misses nothing that came during it, and news the addon never sent is
// read with the next.
func TestOutboxReader(t *testing.T) {
	h, _ := startGame(t, gameConfig(`,"rate":{"per_minute":600,"burst":50}`), t.TempDir())
	c := chatID(t, h, inA)
	react := func(id string) map[string]any {
		return map[string]any{"id": id, "op": "react", "ref": c.Ref, "emoji": "👍"}
	}
	queue(t, h, 0, react("1")) // its news lost
	time.Sleep(100 * time.Millisecond)
	if n := len(h.Reactions()); n != 0 {
		t.Fatalf("%d reactions without news", n)
	}
	queue(t, h, 1, react("2"))
	eventually(t, "both reactions", func() bool { return len(h.Reactions()) == 2 })

	var ops []map[string]any
	for i := range 7 {
		ops = append(ops, react(strconv.Itoa(10+i)))
	}
	queue(t, h, 20, ops...)
	eventually(t, "every reaction", func() bool { return len(h.Reactions()) == 9 })
	time.Sleep(100 * time.Millisecond)
	if n := len(h.Reactions()); n != 9 {
		t.Fatalf("%d reactions", n)
	}
	for _, l := range gameLog(t, "reading ") {
		if l != "1" {
			t.Fatalf("%s reads at once", l)
		}
	}
}

// Posts and reactions are rate limited per chat: past the burst, dropped
// and counted.
func TestOutboxRate(t *testing.T) {
	h, log := startGame(t, gameConfig(`,"rate":{"per_minute":1,"burst":2}`), t.TempDir())
	c := chatID(t, h, inA)
	var ops []map[string]any
	for i := range 3 {
		ops = append(ops, map[string]any{"id": strconv.Itoa(i), "op": "react", "ref": c.Ref, "emoji": "👍"})
	}
	queue(t, h, 1, ops...)
	eventually(t, "the drop", func() bool { return strings.Contains(log.String(), "dropped 1 op: 1 reactions over the rate limit") })
	if n := len(h.Reactions()); n != 2 {
		t.Fatalf("%d reactions", n)
	}
}

// An unwatch ends a watch; one that comes first ends it before it begins.
func TestOutboxUnwatch(t *testing.T) {
	h, log := startGame(t, gameConfig(""), t.TempDir())
	ctx := context.Background()
	c := chatID(t, h, inA)
	round(t, h, inA, 1000, spec("r1", time.Hour, ""))
	queue(t, h, 1, map[string]any{"id": "1", "op": "unwatch", "chat": c.Chat, "watch": "r1"},
		map[string]any{"id": "2", "op": "unwatch", "chat": c.Chat, "watch": "r2"})
	eventually(t, "the unwatch", func() bool { return h.Message(ctx, said(inA, "bob-id", 1001, "guess 5")).IsZero() })
	round(t, h, inA, 1002, spec("r2", time.Hour, ""))
	if got := h.Message(ctx, said(inA, "bob-id", 1003, "guess 5")); !got.IsZero() {
		t.Fatalf("a watch ended before it began: %+v", got)
	}
	if !strings.Contains(log.String(), "a watch refused: the addon ended it before it began") {
		t.Fatalf("log %q", log.String())
	}
}

// --- in memory ------------------------------------------------------------------

// unitAddon is an addon with no server, allowed everywhere, on a clock the
// test moves.
func unitAddon(cfg addonConfig) (*addon, *time.Time) {
	a := newAddon("game", "Game", cfg, bytes.Repeat([]byte{7}, secretSize), nil, map[string]bool{"guess": true})
	now := time.Unix(1800000000, 0)
	a.now = func() time.Time { return now }
	return a, &now
}

var everything = addonConfig{Chats: []string{"all"}, DMs: true, Watch: true, Post: true, React: true, watchMax: time.Hour, Rate: rateConfig{20, 5}}

// specAt is a watch spec ending at until.
func specAt(id string, until time.Time) watchSpec {
	return watchSpec{ID: id, Pattern: `\bguess \d+`, Until: until.Format(time.RFC3339), Tool: "guess"}
}

// A chat has at most maxWatches; a claim lasts claimLife, a watch until its
// end, a tombstone tombLife.
func TestWatchLimits(t *testing.T) {
	var log logx.Buffer
	t.Cleanup(logx.SetOutput(&log, &log, true))
	a, now := unitAddon(everything)
	cmd := said(inA, "alice-id", 1000, "/round")
	for i := range maxWatches {
		if a.watch(cmd, specAt(strconv.Itoa(i), now.Add(time.Minute))) == nil {
			t.Fatalf("watch %d refused", i)
		}
	}
	if a.watch(cmd, specAt("9", now.Add(time.Minute))) != nil || !strings.Contains(log.String(), "8 watches are already live in the chat") {
		t.Fatalf("a 9th: %q", log.String())
	}
	if a.watch(cmd, specAt("0", now.Add(time.Minute))) == nil {
		t.Fatal("one replacing another of its ID was refused")
	}
	if a.watch(said(inB, "alice-id", 1000, "/round"), specAt("0", now.Add(time.Minute))) == nil {
		t.Fatal("refused in another chat")
	}

	bobs := said(inA, "bob-id", 1001, "guess 1")
	if !a.claim(bobs).Done {
		t.Fatal("not claimed")
	}
	*now = now.Add(time.Minute)
	if !a.claim(said(inA, "bob-id", 1002, "guess 2")).IsZero() {
		t.Fatal("claimed after the watch ended")
	}
	if !a.claim(edit(bobs, 1003, "guess 3")).Done {
		t.Fatal("an edit not claimed after the watch ended")
	}
	*now = now.Add(claimLife)
	if !a.claim(edit(bobs, 1004, "guess 4")).IsZero() {
		t.Fatal("an edit claimed after its claim ended")
	}
	if s := a.state(); len(s.Watches) != 0 || len(s.Claims) != 0 || len(s.Leases) != 0 {
		t.Fatalf("kept %+v", s)
	}

	a.unwatch(a.chatID(inA), "late")
	*now = now.Add(tombLife + time.Second)
	if a.watch(cmd, specAt("late", now.Add(time.Minute))) == nil {
		t.Fatal("a tombstone outlived tombLife")
	}
}

// An original is taken whenever the bot gets it, even by a clock behind the
// command's; an edit only of an original from the command on.
func TestWatchClock(t *testing.T) {
	a, now := unitAddon(everything)
	a.watch(said(inA, "alice-id", 1000, "/round"), specAt("r", now.Add(time.Minute)))
	if !a.claim(said(inA, "bob-id", 900, "guess 1")).Done {
		t.Fatal("an original by a slow clock not taken")
	}
	if !a.claim(edit(said(inA, "bob-id", 999, "hello"), 1100, "guess 2")).IsZero() {
		t.Fatal("an older message's edit taken")
	}
}

// fakeConn is a server for the watch worker: it records the texts it is
// sent, or fails as down while down is set.
type fakeConn struct {
	mu    sync.Mutex
	down  bool
	texts []string
	fails int
}

func (c *fakeConn) call(_ context.Context, _ string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down {
		c.fails++
		return nil, errNotRunning
	}
	c.texts = append(c.texts, params.(map[string]any)["arguments"].(map[string]string)["text"])
	return json.RawMessage(`{"content":[]}`), nil
}

func (c *fakeConn) notify(context.Context, string, any) error { return nil }

func (c *fakeConn) state() ([]string, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.texts), c.fails
}

// A chat past its rate doesn't hold up another's matches; a match the
// server was down for is called again once it has started.
func TestWatchWorker(t *testing.T) {
	defer func(r rateConfig) { watchRate = r }(watchRate)
	watchRate = rateConfig{PerMinute: 1, Burst: 1}
	a, now := unitAddon(everything)
	c := &fakeConn{}
	a.conn = c
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.work(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	for _, chat := range []feature.Chat{inA, inB, inDM} {
		a.watch(said(chat, "alice-id", 1000, "/round"), specAt("r", now.Add(time.Minute)))
	}
	a.claim(said(inA, "bob-id", 1001, "guess 1"))
	a.claim(said(inA, "bob-id", 1002, "guess 2")) // due in a minute
	a.claim(said(inB, "bob-id", 1003, "guess 3"))
	eventually(t, "the calls", func() bool { texts, _ := c.state(); return len(texts) == 2 })
	if texts, _ := c.state(); !slices.Equal(texts, []string{"guess 1", "guess 3"}) {
		t.Fatalf("called with %q", texts)
	}

	c.mu.Lock()
	c.down = true
	c.mu.Unlock()
	a.claim(said(inDM, "bob-id", 1004, "guess 4"))
	eventually(t, "the failed call", func() bool { _, fails := c.state(); return fails == 1 })
	c.mu.Lock()
	c.down = false
	c.mu.Unlock()
	time.Sleep(50 * time.Millisecond)
	if texts, _ := c.state(); len(texts) != 2 {
		t.Fatalf("called again before the server started: %q", texts)
	}
	a.up <- struct{}{}
	eventually(t, "the call again", func() bool { texts, _ := c.state(); return len(texts) == 3 })
	if texts, _ := c.state(); texts[2] != "guess 4" {
		t.Fatalf("called with %q", texts)
	}
}

// A match never waits for the watch worker: past maxMatches waiting, it is
// dropped with a warning, though still taken.
func TestWatchQueueFull(t *testing.T) {
	var log logx.Buffer
	t.Cleanup(logx.SetOutput(&log, &log, true))
	a, now := unitAddon(everything)
	a.watch(said(inA, "alice-id", 1000, "/round"), specAt("r", now.Add(time.Minute)))
	for i := range maxMatches + 1 {
		if !a.claim(said(inA, "bob-id", int64(1001+i), "guess 1")).Done {
			t.Fatal("not claimed")
		}
	}
	if strings.Count(log.String(), "<4>addon game: watch queue full, a matched message dropped\n") != 1 {
		t.Fatalf("log %q", log.String())
	}
	if s := a.state(); len(s.Claims) != maxMatches+1 {
		t.Fatalf("%d claims", len(s.Claims))
	}
}

// Past the watch rate, a call waits its turn rather than being dropped.
func TestWatchRate(t *testing.T) {
	defer func(r rateConfig) { watchRate = r }(watchRate)
	watchRate = rateConfig{PerMinute: 600, Burst: 1} // one each 100ms
	h, _ := startGame(t, gameConfig(""), t.TempDir())
	ctx := context.Background()
	round(t, h, inA, 1000, spec("r1", time.Hour, ""))
	start := time.Now()
	for i := range 4 {
		h.Message(ctx, said(inA, "bob-id", int64(1001+i), "guess "+strconv.Itoa(i)+" nopin"))
	}
	eventually(t, "every guess", func() bool { return len(guesses(t)) == 4 })
	if d := time.Since(start); d < 250*time.Millisecond {
		t.Fatalf("4 calls in %s", d)
	}
}

func TestLimiter(t *testing.T) {
	l := newLimiter(rateConfig{PerMinute: 60, Burst: 2})
	now := time.Unix(0, 0)
	if !l.allow("a", now) || !l.allow("a", now) || l.allow("a", now) || !l.allow("b", now) {
		t.Fatal("burst")
	}
	if !l.allow("a", now.Add(time.Second)) || l.allow("a", now.Add(time.Second)) {
		t.Fatal("refill")
	}
	if w := l.take("c", now); w != 0 {
		t.Fatalf("waits %s", w)
	}
	l.take("c", now)
	if w := l.take("c", now); w != time.Second {
		t.Fatalf("waits %s", w)
	}
}

// Catch-alls and patterns for ordinary chat are refused; a game's are not.
func TestOrdinary(t *testing.T) {
	for _, p := range []string{`.*`, `.`, `^`, `x*`, `(?i)lol`, `\d`, `\w+`, `(?i)the`, `https?://`, `\?$`, `[a-z]{3}`} {
		if !ordinary(regexp.MustCompile(p)) {
			t.Errorf("%s passed", p)
		}
	}
	geo := `(?i)(?:\bmaps\.google\.[a-z.]+/|\bgoogle\.[a-z.]+/maps|\bmaps\.apple\.com\b|\bgeo:[+-]?\d|\bopenstreetmap\.org/|(?:^|[^\w.])-?\d{1,3}\.\d+°?\s*,\s*-?\d{1,3}\.\d+)`
	for _, p := range []string{geo, `\bguess \d+`, `^/vote [a-d]$`} {
		if ordinary(regexp.MustCompile(p)) {
			t.Errorf("%s refused", p)
		}
	}
	if n := len(chatter); n < 200 {
		t.Errorf("%d lines of chatter", n)
	}
}

func TestNotFound(t *testing.T) {
	for code, want := range map[int]bool{-32002: true, -32602: true, -32601: false} {
		if got := errNotFound(fmt.Errorf("x: %w", &rpcError{Code: code})); got != want {
			t.Errorf("%d: %v", code, got)
		}
	}
	if errNotFound(context.DeadlineExceeded) {
		t.Error("a timeout")
	}
}

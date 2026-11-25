package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// feedEvery is how often a feed is read; tests shorten it.
var feedEvery = 30 * time.Second

const (
	// maxSeen is how many item IDs a feed remembers past those it still
	// offers, all of which it remembers.
	maxSeen = 1000
	// maxNew is how many new items one read sends; the rest are skipped,
	// so a server that floods can't spam its chats.
	maxNew = 10
	// newsFor is how long an item without "expires" is news, after its
	// "time".
	newsFor = time.Hour
	// pressFeed starts a feed menu's buttons' data: then the feed's
	// command, "/", what the button does, and, in a forum topic, "/" and
	// the topic, which a press on an older message doesn't name.
	pressFeed = "feed="
	// maxID is the longest item ID kept, in bytes; an item with a longer
	// one is skipped.
	maxID = 200
	// maxLink is the longest link posted, in runes.
	maxLink = 500
	// maxKinds is how many kinds a feed's menu shows.
	maxKinds = 20
)

// feedConfig is a server's "feed": a resource the bot reads every
// feedEvery, and the command chats subscribe to it with.
type feedConfig struct {
	Resource string `json:"resource"`
	Command  string `json:"command"` // unset: "alerts"
}

// feedDoc is what a feed resource's text says, as docs/FEEDS.md has it.
type feedDoc struct {
	Identity string     `json:"identity"` // changes when the server's IDs start over
	Kinds    []feedKind `json:"kinds"`    // the kinds a chat may turn off
	Items    []feedItem `json:"items"`    // oldest first
}

type feedKind struct {
	Kind  string   `json:"kind"`
	Label string   `json:"label"`
	Words []string `json:"words"` // more words that name it, after /alerts
}

type feedItem struct {
	ID      string    `json:"id"`
	Kind    string    `json:"kind"`
	Time    time.Time `json:"time"`
	Expires time.Time `json:"expires"`
	Text    string    `json:"text"`
	Link    string    `json:"link"`
	Chart   *struct {
		Tool      string         `json:"tool"`
		Arguments map[string]any `json:"arguments"`
		Arg       string         `json:"arg"`    // the argument a button may change
		Values    []string       `json:"values"` // the values it may take, the buttons
	} `json:"chart"`
}

// expiry is when it stops being news; zero if it never says.
func (it feedItem) expiry() time.Time {
	switch {
	case !it.Expires.IsZero():
		return it.Expires
	case !it.Time.IsZero():
		return it.Time.Add(newsFor)
	}
	return time.Time{}
}

// feeds are the servers' feeds, and the chats subscribed to each, kept in
// one store.
type feeds struct {
	h     feature.Host
	store feature.Store
	now   func() time.Time
	again *reruns // with buttons: where a chart's post is kept for them

	mu    sync.Mutex
	doc   map[string]*feedState // by server
	byCmd map[string]*feed      // by command
}

// feedState is a feed's saved state.
type feedState struct {
	Identity string              `json:"identity,omitempty"`
	Seen     []string            `json:"seen,omitempty"`     // the IDs it has had, oldest first
	Started  bool                `json:"started,omitempty"`  // it has been read once
	Chats    map[string]*feedSub `json:"chats,omitempty"`    // the chats subscribed, by key
	Declined map[string]bool     `json:"declined,omitempty"` // DMs that turned alerts off, by key, so none is subscribed again unasked
}

// feedSub is a chat's subscription.
type feedSub struct {
	Chat  feature.Chat `json:"chat"`
	Off   []string     `json:"off,omitempty"` // kinds it doesn't want
	Sound bool         `json:"sound,omitempty"`
}

// feed is a server's feed.
type feed struct {
	server, title, command, resource string
	conn                             conn
	has                              map[string]bool // the server's tools, which a chart may call
	kinds                            []feedKind      // as the last read had them; under feeds.mu
	failing                          bool            // its last read failed
}

func newFeeds(h feature.Host, store feature.Store) *feeds {
	fs := &feeds{h: h, store: store, now: time.Now, doc: map[string]*feedState{}, byCmd: map[string]*feed{}}
	if _, err := store.Load(&fs.doc); err != nil || fs.doc == nil {
		if err != nil {
			logx.Warnf("MCP: the feeds' state can't be read, so they start afresh: %v", err)
		}
		fs.doc = map[string]*feedState{}
	}
	return fs
}

// save writes the state; under mu.
func (fs *feeds) save() {
	if err := fs.store.Save(fs.doc); err != nil {
		logx.Warnf("MCP: the feeds' state can't be saved: %v", err)
	}
}

// state is server's state, made if new; under mu.
func (fs *feeds) state(server string) *feedState {
	st := fs.doc[server]
	if st == nil {
		st = &feedState{}
		fs.doc[server] = st
	}
	if st.Chats == nil {
		st.Chats = map[string]*feedSub{}
	}
	if st.Declined == nil {
		st.Declined = map[string]bool{}
	}
	return st
}

// add starts server's feed, read every feedEvery, and registers its
// command, returned as "/name", or "" if it can't be.
func (fs *feeds) add(server, title string, cfg feedConfig, c conn, has map[string]bool) string {
	f := &feed{server: server, title: title, command: strings.ToLower(commandName(cfg.Command, "alerts")), resource: cfg.Resource, conn: c, has: has}
	err := fs.h.Command(f.command, func(_ context.Context, m feature.Message, args string) feature.Action {
		return fs.chat(f, m.Chat, args)
	})
	if err != nil {
		logx.Warnf("MCP server %s: the feed's command skipped: %v", server, err)
		return ""
	}
	fs.mu.Lock()
	fs.byCmd[f.command] = f
	if fs.again != nil {
		fs.again.charts[server] = func(ctx context.Context, tool string, args map[string]any) []feature.Picture {
			return fs.chart(ctx, f, tool, args)
		}
	}
	fs.mu.Unlock()
	fs.h.Go(func(ctx context.Context) {
		t := time.NewTicker(feedEvery)
		defer t.Stop()
		for {
			fs.poll(ctx, f)
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	})
	return "/" + f.command
}

// commandName is s without its slash, or def if s is empty.
func commandName(s, def string) string {
	if s = strings.TrimPrefix(strings.TrimSpace(s), "/"); s != "" {
		return s
	}
	return def
}

// poll reads f, and sends its new items to the chats that want them.
func (fs *feeds) poll(ctx context.Context, f *feed) {
	cctx, cancel := context.WithTimeout(ctx, directTimeout)
	res, err := f.conn.call(cctx, "resources/read", map[string]any{"uri": f.resource})
	cancel()
	var r directResult
	var doc feedDoc
	why := ""
	switch {
	case err != nil:
		why = errCode(err)
	case json.Unmarshal(res, &r) != nil || len(r.Contents) == 0:
		why = "a malformed result"
	case json.Unmarshal([]byte(r.Contents[0].Text), &doc) != nil:
		why = "text that isn't a feed"
	}
	fs.mu.Lock()
	if why != "" {
		if !f.failing && ctx.Err() == nil {
			logx.Warnf("MCP server %s: the feed can't be read: %s", f.server, why)
		}
		f.failing = true
		fs.mu.Unlock()
		return
	}
	if f.failing {
		logx.Infof("MCP server %s: the feed can be read again", f.server)
	}
	f.failing = false
	var kinds []feedKind // those a button can name: short, without "/"
	for _, k := range doc.Kinds {
		if k.Kind != "" && len(k.Kind) <= maxLabel && !strings.Contains(k.Kind, "/") && len(kinds) < maxKinds {
			k.Label = clampLabel(strings.TrimSpace(k.Label))
			kinds = append(kinds, k)
		}
	}
	f.kinds = kinds
	st := fs.state(f.server)
	if doc.Identity != st.Identity {
		if st.Started {
			logx.Noticef("MCP server %s: the feed started over, so its items are new again", f.server)
		}
		st.Identity, st.Seen = doc.Identity, nil
	}
	first, now := !st.Started, fs.now()
	seen := map[string]bool{}
	for _, id := range st.Seen {
		seen[id] = true
	}
	var fresh []feedItem
	changed := first
	items := doc.Items[max(0, len(doc.Items)-maxSeen):] // the newest, so what is kept stays bounded
	for _, it := range items {
		if it.ID == "" || len(it.ID) > maxID || seen[it.ID] {
			continue
		}
		seen[it.ID], changed = true, true
		st.Seen = append(st.Seen, it.ID)
		exp := it.expiry()
		if exp.IsZero() && first || !exp.IsZero() && !now.Before(exp) || strings.TrimSpace(it.Text) == "" {
			continue // on the first read, an item that says no time could be old
		}
		fresh = append(fresh, it)
	}
	st.Started = true
	st.Seen = trimSeen(st.Seen, items)
	if len(fresh) > maxNew {
		logx.Warnf("MCP server %s: the feed had %d new items; sent the newest %d", f.server, len(fresh), maxNew)
		fresh = fresh[len(fresh)-maxNew:]
	}
	subs := make([]feedSub, 0, len(st.Chats))
	for _, s := range st.Chats {
		c := *s
		c.Off = slices.Clone(s.Off) // set changes it in place, after mu is let go
		subs = append(subs, c)
	}
	if changed {
		fs.save()
	}
	fs.mu.Unlock()
	for _, it := range fresh {
		fs.send(ctx, f, it, subs)
	}
}

// trimSeen is seen, oldest first, keeping every ID items still lists, so
// an item that stays listed for long is never sent again, and maxSeen of
// the others, the newest.
func trimSeen(seen []string, items []feedItem) []string {
	listed := map[string]bool{}
	for _, it := range items {
		listed[it.ID] = true
	}
	others := 0
	for _, id := range seen {
		if !listed[id] {
			others++
		}
	}
	drop := others - maxSeen
	return slices.DeleteFunc(seen, func(id string) bool {
		if drop > 0 && !listed[id] {
			drop--
			return true
		}
		return false
	})
}

// send posts it to the chats among subs that want its kind, with its
// chart, if it has one, drawn once.
func (fs *feeds) send(ctx context.Context, f *feed, it feedItem, subs []feedSub) {
	var want []feedSub
	for _, s := range subs {
		if !slices.Contains(s.Off, it.Kind) {
			want = append(want, s)
		}
	}
	if len(want) == 0 {
		return
	}
	text := txt.Clamp(strings.TrimSpace(it.Text), maxAnswer)
	if link := strings.TrimSpace(it.Link); link != "" {
		text += "\n" + txt.Clamp(link, maxLink)
	}
	var pics []feature.Picture
	if it.Chart != nil {
		pics = fs.chart(ctx, f, it.Chart.Tool, it.Chart.Arguments)
	}
	for _, s := range want {
		p := feature.Post{Chat: s.Chat, Label: f.title, Text: text, Pictures: pics, Markup: feature.Markup{Silent: !s.Sound}}
		if fs.again != nil && len(pics) > 0 {
			fs.again.chart(&p, f.server, it.Chart.Tool, maps.Clone(it.Chart.Arguments), it.Chart.Arg, it.Chart.Values)
		}
		fs.h.Post(p)
	}
}

// chart is the pictures tool draws with args, if f's server has it; none
// if it can't.
func (fs *feeds) chart(ctx context.Context, f *feed, tool string, args map[string]any) []feature.Picture {
	if !f.has[tool] {
		logx.Warnf("MCP server %s: a feed item's chart names tool %q, which the server doesn't have", f.server, txt.Clamp(tool, 64))
		return nil
	}
	if args == nil {
		args = map[string]any{}
	}
	ctx, cancel := context.WithTimeout(ctx, directTimeout)
	defer cancel()
	res, err := f.conn.call(ctx, "tools/call", map[string]any{"name": tool, "arguments": args})
	var r directResult
	if err != nil || json.Unmarshal(res, &r) != nil || r.IsError {
		logx.Warnf("MCP server %s: a feed item's chart couldn't be drawn", f.server)
		return nil
	}
	return r.action(f.server).Pictures
}

// chat answers /alerts with args in chat: on, off, sound on or off, or a
// kind to turn off or on; the menu either way.
func (fs *feeds) chat(f *feed, chat feature.Chat, args string) feature.Action {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.adopt(f, chat)
	words := strings.Fields(strings.ToLower(args))
	reply := ""
	switch {
	case len(words) == 0:
	case len(words) == 1 && (words[0] == "on" || words[0] == "off"):
		fs.set(f, chat, words[0])
	case len(words) == 2 && words[0] == "sound" && (words[1] == "on" || words[1] == "off"):
		fs.set(f, chat, "sound"+words[1])
	default:
		k := f.kind(strings.Join(words, " "))
		if k == "" {
			reply = "I don't know that one. "
			break
		}
		fs.set(f, chat, "k:"+k)
	}
	text, buttons := fs.menu(f, chat)
	return feature.Action{Answer: reply + text, Markup: feature.Markup{Buttons: buttons}}
}

// adopt subscribes chat to f, every kind, silently, if it is a DM that
// never was and never turned alerts off: alerts are on by default in DMs,
// from the first message to the bot. Groups choose for themselves. Under
// mu.
func (fs *feeds) adopt(f *feed, chat feature.Chat) {
	if chat.GroupID != "" || chat.User == "" {
		return
	}
	st, key := fs.state(f.server), chat.Key()
	if st.Chats[key] != nil || st.Declined[key] {
		return
	}
	st.Chats[key] = &feedSub{Chat: chat}
	fs.save()
}

// seen adopts the DM of m into every feed, as any message there shows the
// bot is used there; it never claims m.
func (fs *feeds) seen(_ context.Context, m feature.Message) feature.Action {
	if m.Chat.GroupID == "" && m.Addressed {
		fs.mu.Lock()
		for _, f := range fs.byCmd {
			fs.adopt(f, m.Chat)
		}
		fs.mu.Unlock()
	}
	return feature.Action{}
}

// kind is the kind word names, by its kind, label or words; "" if none.
func (f *feed) kind(word string) string {
	for _, k := range f.kinds {
		if strings.EqualFold(word, k.Kind) || strings.EqualFold(word, k.Label) || slices.ContainsFunc(k.Words, func(w string) bool { return strings.EqualFold(w, word) }) {
			return k.Kind
		}
	}
	return ""
}

// set does what to chat's subscription: on, off, soundon, soundoff, or
// "k:" and a kind to turn off, or on again; in a chat with alerts off, a
// kind turns them on for that kind alone. Under mu.
func (fs *feeds) set(f *feed, chat feature.Chat, what string) {
	st := fs.state(f.server)
	key := chat.Key()
	s := st.Chats[key]
	if what == "off" {
		delete(st.Chats, key)
		if chat.GroupID == "" {
			st.Declined[key] = true
		}
		fs.save()
		return
	}
	delete(st.Declined, key) // asked for some: no longer declined
	if s == nil {
		s = &feedSub{Chat: chat}
		st.Chats[key] = s
		if kind, ok := strings.CutPrefix(what, "k:"); ok { // this kind alone
			for _, k := range f.kinds {
				if k.Kind != kind {
					s.Off = append(s.Off, k.Kind)
				}
			}
			fs.save()
			return
		}
	}
	switch kind, ok := strings.CutPrefix(what, "k:"); {
	case ok && slices.Contains(s.Off, kind):
		s.Off = slices.DeleteFunc(s.Off, func(k string) bool { return k == kind })
	case ok:
		s.Off = append(s.Off, kind)
	case what == "soundon", what == "soundoff":
		s.Sound = what == "soundon"
	}
	fs.save()
}

// menu is the chat's subscription in words, and, where the network has
// buttons, the buttons that change it; under mu.
func (fs *feeds) menu(f *feed, chat feature.Chat) (string, [][]feature.Button) {
	s := fs.state(f.server).Chats[chat.Key()]
	var b strings.Builder
	cmd := "/" + f.command
	if s == nil {
		fmt.Fprintf(&b, "%s alerts are off here.", f.title)
		if len(f.kinds) > 0 {
			var names []string
			for _, k := range f.kinds {
				names = append(names, kindLabel(k))
			}
			fmt.Fprintf(&b, " There are: %s. Turn them all on, or choose one.", strings.Join(names, ", "))
		}
	} else {
		fmt.Fprintf(&b, "%s alerts are on here, %s.", f.title, map[bool]string{true: "with sound", false: "silently"}[s.Sound])
		var on, off []string
		for _, k := range f.kinds {
			if slices.Contains(s.Off, k.Kind) {
				off = append(off, kindLabel(k))
			} else {
				on = append(on, kindLabel(k))
			}
		}
		if len(on) > 0 {
			fmt.Fprintf(&b, " On: %s.", strings.Join(on, ", "))
		}
		if len(off) > 0 {
			fmt.Fprintf(&b, " Off: %s.", strings.Join(off, ", "))
		}
	}
	if !fs.h.Net(chat).Caps.Buttons {
		fmt.Fprintf(&b, "\n%s on or off; %s sound on or off", cmd, cmd)
		if len(f.kinds) > 0 {
			var names []string
			for _, k := range f.kinds {
				names = append(names, strings.ToLower(k.Kind))
			}
			fmt.Fprintf(&b, "; %s and a kind to turn it off or on (with alerts off, on alone): %s", cmd, strings.Join(names, ", "))
		}
		b.WriteString(".")
		return b.String(), nil
	}
	data := func(what string) string {
		d := pressFeed + f.command + "/" + what
		if _, topic, ok := strings.Cut(chat.GroupID, "/"); ok {
			d += "/" + topic
		}
		return d
	}
	rows := [][]feature.Button{{{Label: "Turn all on", Data: data("on")}}}
	if s != nil {
		sound := feature.Button{Label: "🔔 Sound on", Data: data("soundon")}
		if s.Sound {
			sound = feature.Button{Label: "🔕 Silent", Data: data("soundoff")}
		}
		rows = [][]feature.Button{{{Label: "Turn off", Data: data("off")}, sound}}
	}
	var row []feature.Button
	for _, k := range f.kinds {
		if len(data("k:"+k.Kind)) > maxData || len(rows) == maxRows {
			continue
		}
		mark := "✅ "
		if s == nil || slices.Contains(s.Off, k.Kind) {
			mark = "▫️ "
		}
		row = append(row, feature.Button{Label: clampLabel(mark + kindLabel(k)), Data: data("k:" + k.Kind)})
		if len(row) == maxPerRow {
			rows, row = append(rows, row), nil
		}
	}
	if len(row) > 0 && len(rows) < maxRows {
		rows = append(rows, row)
	}
	return b.String(), rows
}

func kindLabel(k feedKind) string {
	if k.Label != "" {
		return k.Label
	}
	return k.Kind
}

// press changes a chat's subscription by a menu's button, and redraws the
// menu in place.
func (fs *feeds) press(_ context.Context, p feature.Press) (string, bool) {
	rest, ok := strings.CutPrefix(p.Data, pressFeed)
	parts := strings.Split(rest, "/")
	if !ok || len(parts) < 2 || len(parts) > 3 {
		return "", false
	}
	cmd, what := parts[0], parts[1]
	if len(parts) == 3 && p.Chat.GroupID != "" && !strings.Contains(p.Chat.GroupID, "/") {
		p.Chat.GroupID += "/" + parts[2] // the topic the menu was posted in
	}
	fs.mu.Lock()
	defer fs.mu.Unlock()
	f := fs.byCmd[cmd]
	if f == nil || !strings.HasPrefix(what, "k:") && !slices.Contains([]string{"on", "off", "soundon", "soundoff"}, what) {
		return "That button no longer works.", true
	}
	if k, isKind := strings.CutPrefix(what, "k:"); isKind && !slices.ContainsFunc(f.kinds, func(x feedKind) bool { return x.Kind == k }) {
		return "That choice no longer works.", true
	}
	fs.set(f, p.Chat, what)
	text, buttons := fs.menu(f, p.Chat)
	fs.h.Post(feature.Post{Chat: p.Chat, Text: text, Edit: p.Sent, Markup: feature.Markup{Buttons: buttons}})
	return "", true
}

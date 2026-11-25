package slack

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/img"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/mdstyle"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// event is an Events API event: the parts the bot reads.
type event struct {
	Type        string    `json:"type"`
	Subtype     string    `json:"subtype"`
	Channel     string    `json:"channel"`
	ChannelType string    `json:"channel_type"` // im, mpim, channel or group
	User        string    `json:"user"`
	BotID       string    `json:"bot_id"`
	Text        string    `json:"text"`
	TS          string    `json:"ts"`
	ThreadTS    string    `json:"thread_ts"`
	Files       []file    `json:"files"`
	Hidden      bool      `json:"hidden"`
	Message     *event    `json:"message"`          // message_changed's new version
	Previous    *event    `json:"previous_message"` // message_changed's and message_deleted's old one
	DeletedTS   string    `json:"deleted_ts"`
	Reaction    string    `json:"reaction"`
	ItemUser    string    `json:"item_user"`
	Item        *struct { // what a reaction is on
		Type    string `json:"type"`
		Channel string `json:"channel"`
		TS      string `json:"ts"`
	} `json:"item"`
	Inviter string `json:"inviter"`
	Profile *struct {
		DisplayName string `json:"display_name"`
		RealName    string `json:"real_name"`
	} `json:"user_profile"`
}

type file struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	MimeType string `json:"mimetype"`
	Size     int    `json:"size"`
}

// action is an interactive payload: a press of a button under one of the
// bot's messages.
type action struct {
	Type string `json:"type"`
	User struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"user"`
	Channel struct {
		ID string `json:"id"`
	} `json:"channel"`
	Container struct {
		MessageTS string `json:"message_ts"`
		ThreadTS  string `json:"thread_ts"`
	} `json:"container"`
	Message *struct {
		TS       string `json:"ts"`
		ThreadTS string `json:"thread_ts"`
	} `json:"message"`
	Actions []struct {
		Value string `json:"value"`
	} `json:"actions"`
}

// slash is a slash command's payload.
type slash struct {
	Command   string `json:"command"`
	Text      string `json:"text"`
	ChannelID string `json:"channel_id"`
	UserID    string `json:"user_id"`
	UserName  string `json:"user_name"`
}

// tsInt is Slack's ts, "1700000000.000200", as microseconds, the int64 the
// core names messages by; 0 if it can't be read.
func tsInt(ts string) int64 {
	sec, frac, _ := strings.Cut(ts, ".")
	n, err := strconv.ParseInt(sec, 10, 64)
	f, ferr := strconv.ParseInt((frac + "000000")[:6], 10, 64)
	if err != nil || ferr != nil || n <= 0 {
		return 0
	}
	return n*1_000_000 + f
}

// tsString is tsInt's inverse, with Slack's six decimals.
func tsString(n int64) string { return fmt.Sprintf("%d.%06d", n/1_000_000, n%1_000_000) }

// chatIn is the chat of a message in channel, its DM channel_type, and
// thread: a DM with its user, a channel, or a thread in it, which is a
// conversation of its own. user is who posted it.
func (c *Client) chatIn(channel, channelType, thread, ts, user string) feature.Chat {
	if channelType == "im" || strings.HasPrefix(channel, "D") {
		c.mu.Lock()
		if u := c.imUser[channel]; u != "" {
			user = u
		} else if user != "" && user != c.selfID() {
			c.noteIM(user, channel)
		}
		c.mu.Unlock()
		if user == "" || user == c.selfID() {
			return feature.Chat{}
		}
		return feature.Chat{User: prefix + user}
	}
	if thread != "" && thread != ts {
		return feature.Chat{GroupID: prefix + channel + "/" + thread}
	}
	return feature.Chat{GroupID: prefix + channel}
}

// noteIM remembers user's DM channel with the bot, under c.mu.
func (c *Client) noteIM(user, channel string) {
	if _, ok := c.ims[user]; ok || len(c.ims) < 10000 {
		c.ims[user], c.imUser[channel] = channel, user
	}
}

// events is env as the core gets it; none if the core has nothing to do
// with it.
func (c *Client) events(env envelope) []bot.Received {
	switch env.Type {
	case "events_api":
		var p struct {
			Event event `json:"event"`
		}
		if json.Unmarshal(env.Payload, &p) != nil {
			return nil
		}
		return c.event(&p.Event)
	case "interactive":
		var a action
		if json.Unmarshal(env.Payload, &a) != nil || a.Type != "block_actions" {
			return nil
		}
		return c.pressed(&a)
	case "slash_commands":
		var s slash
		if json.Unmarshal(env.Payload, &s) != nil {
			return nil
		}
		return c.command(&s)
	}
	return nil
}

func one(r bot.Received, ok bool) []bot.Received {
	if !ok {
		return nil
	}
	return []bot.Received{r}
}

// event is an Events API event as the core gets it, decided without waiting
// on the network.
func (c *Client) event(e *event) []bot.Received {
	switch e.Type {
	case "message":
		switch e.Subtype {
		case "", "file_share", "thread_broadcast", "me_message":
			return one(c.received(e, e, 0))
		case "message_changed":
			if m := e.Message; m != nil && (m.Subtype == "tombstone" || m.Hidden) {
				return one(c.tombstone(e))
			}
			if m, p := e.Message, e.Previous; m != nil && (p == nil || m.Text != p.Text || len(m.Files) != len(p.Files)) { // not an unfurl
				return one(c.received(e, m, tsInt(e.TS)))
			}
		case "message_deleted":
			return one(c.deleted(e))
		}
	case "reaction_added", "reaction_removed":
		if c.watch.Load() {
			return one(c.reacted(e))
		}
	case "member_joined_channel", "member_left_channel":
		return c.membership(e)
	}
	return nil
}

// received is m, a message in e's channel, or with edited an edit of one,
// as the core gets it: the bot's own and other bots' are skipped.
func (c *Client) received(e, m *event, edited int64) (bot.Received, bool) {
	self := c.selfID()
	if m.BotID != "" || m.Subtype == "bot_message" || m.User == "" || self == "" || m.User == self {
		return nil, false
	}
	ch := c.chatIn(e.Channel, e.ChannelType, m.ThreadTS, m.TS, m.User)
	if ch == (feature.Chat{}) {
		return nil, false
	}
	if ch.GroupID != "" {
		c.seen(e.Channel, m.User)
		if m.ThreadTS != "" && m.ThreadTS != m.TS {
			c.noteThread(e.Channel, m.TS, m.ThreadTS)
		}
	}
	if m.Profile != nil {
		c.learnName(m.User, cmp.Or(m.Profile.DisplayName, m.Profile.RealName))
	}
	msg := c.message(ch, m, edited)
	switch {
	case msg.Text == "" && len(msg.Media) == 0:
		return nil, false
	case msg.Addressed || m.ThreadTS != "" && m.ThreadTS != m.TS && c.sentByBot(e.Channel, m.ThreadTS):
		msg.Addressed = true
		return bot.Incoming{Chat: ch, Load: func(ctx context.Context, learn func(name, id string)) (feature.Message, bool) {
			c.names(ctx, append([]string{m.User}, mentions(m.Text)...))
			full := c.message(ch, m, edited) // with the names just fetched
			full.Addressed = true
			if ch.GroupID != "" {
				for _, id := range append([]string{m.User}, mentions(m.Text)...) {
					if n := c.nameOf(id); n != "" && id != self {
						learn(n, prefix+id)
					}
				}
			}
			return full, true
		}}, true
	case ch.GroupID != "":
		c.namesLater(append([]string{m.User}, mentions(m.Text)...))
		return bot.Chatter{Message: msg}, true
	}
	return nil, false
}

// message is m as the core and features see it: its text with the bot's
// own mention taken out and other people's as @Name, and its files.
func (c *Client) message(ch feature.Chat, m *event, edited int64) feature.Message {
	text, mentioned := c.plain(m.Text)
	msg := feature.Message{Chat: ch, Sender: prefix + m.User, Name: c.nameOf(m.User), Text: text, Timestamp: tsInt(m.TS), Edited: edited,
		Addressed: ch.GroupID == "" || mentioned}
	var images, files []feature.Media
	for _, f := range m.Files {
		switch {
		case f.ID == "":
		case img.Supported(f.MimeType) && f.Size <= img.MaxReceived && len(images) < feature.MaxPictures:
			images = append(images, feature.Media{ID: f.ID})
		case len(files) < feature.MaxFiles:
			id := f.ID
			if f.Size > img.MaxReceived {
				id = "" // too big to fetch: noted, not fetched
			}
			files = append(files, feature.Media{ID: id, File: true, Name: f.Name, MIME: f.MimeType, Size: f.Size})
		}
	}
	msg.Media = slices.Concat(images, files)
	return msg
}

// markup is Slack's <…> in a message's text: a mention, a channel, a
// special mention or a link.
var markup = regexp.MustCompile(`<([^<>]*)>`)

// mentions are the user IDs text mentions.
func mentions(text string) []string {
	var out []string
	for _, m := range markup.FindAllStringSubmatch(text, -1) {
		if id, ok := strings.CutPrefix(m[1], "@"); ok {
			id, _, _ = strings.Cut(id, "|")
			out = append(out, id)
		}
	}
	return out
}

// plain is a message's text as people read it: mentions as @Name, the
// bot's own taken out (mentioned says it was there), channels as #name,
// links as their text and address, and Slack's escapes undone.
func (c *Client) plain(s string) (string, bool) {
	self, mentioned := c.selfID(), false
	s = markup.ReplaceAllStringFunc(s, func(m string) string {
		in := m[1 : len(m)-1]
		body, label, _ := strings.Cut(in, "|")
		switch {
		case strings.HasPrefix(body, "@"):
			id := body[1:]
			if id == self {
				mentioned = true
				return ""
			}
			return "@" + cmp.Or(c.nameOf(id), txt.SafeName(label), "[unnamed user]")
		case strings.HasPrefix(body, "#"):
			return "#" + cmp.Or(label, "channel")
		case strings.HasPrefix(body, "!"):
			if label != "" {
				return label
			}
			return "@" + strings.TrimPrefix(body, "!")
		case label == "" || label == body:
			return body
		}
		return label + " (" + body + ")"
	})
	s = strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&").Replace(s)
	return strings.TrimSpace(strings.Join(strings.FieldsFunc(s, func(r rune) bool { return r == ' ' }), " ")), mentioned
}

// deleted is a message deleted, or the bot's own.
func (c *Client) deleted(e *event) (bot.Received, bool) {
	p := e.Previous
	if p == nil || e.DeletedTS == "" {
		return nil, false
	}
	self := c.selfID()
	byBot := p.User == self && self != ""
	ch := c.chatIn(e.Channel, e.ChannelType, p.ThreadTS, e.DeletedTS, p.User)
	if ch == (feature.Chat{}) || p.User == "" {
		return nil, false
	}
	return bot.Deleted{Chat: ch, Sender: prefix + p.User, Bot: byBot, Sent: tsInt(e.DeletedTS), At: tsInt(e.TS)}, true
}

// tombstone is a deleted thread parent, which Slack sends as an edit that
// leaves a placeholder by USLACKBOT in its place: a delete of the message
// it replaced, of every version.
func (c *Client) tombstone(e *event) (bot.Received, bool) {
	p := e.Previous
	if p == nil || p.User == "" || p.TS == "" {
		return nil, false
	}
	self := c.selfID()
	ch := c.chatIn(e.Channel, e.ChannelType, p.ThreadTS, p.TS, p.User)
	if ch == (feature.Chat{}) {
		return nil, false
	}
	return bot.Deleted{Chat: ch, Sender: prefix + p.User, Bot: self != "" && p.User == self, Sent: tsInt(p.TS), At: math.MaxInt64}, true
}

// reacted is a reaction as the core gets it, but the bot's own, and one
// with an emoji the table doesn't know.
func (c *Client) reacted(e *event) (bot.Received, bool) {
	emoji := emojiOf(e.Reaction)
	if e.Item == nil || e.Item.Type != "message" || e.User == "" || e.User == c.selfID() || emoji == "" {
		return nil, false
	}
	ch := c.chatIn(e.Item.Channel, "", c.threadOf(e.Item.Channel, e.Item.TS), e.Item.TS, e.User)
	if ch == (feature.Chat{}) {
		return nil, false
	}
	byBot := e.ItemUser != "" && e.ItemUser == c.selfID()
	author := ""
	if e.ItemUser != "" {
		author = prefix + e.ItemUser
	}
	return bot.Happened{Event: feature.Event{Chat: ch, By: c.person(e.User), Reaction: &feature.Reaction{
		Emoji: emoji, Removed: e.Type == "reaction_removed", Author: author, Bot: byBot, Sent: tsInt(e.Item.TS)}}}, true
}

// membership is someone joining or leaving a channel: the bot itself, so it
// posts in a channel only while it is in it, or, for the event hooks,
// someone else.
func (c *Client) membership(e *event) []bot.Received {
	if e.Channel == "" || e.User == "" {
		return nil
	}
	ch := feature.Chat{GroupID: prefix + e.Channel}
	joined := e.Type == "member_joined_channel"
	c.mu.Lock()
	if e.User == c.selfID() {
		if joined {
			delete(c.left, e.Channel)
		} else {
			if len(c.left) < 2000 {
				c.left[e.Channel] = true
			}
			delete(c.chans, e.Channel)
		}
	} else if !joined {
		delete(c.chans[e.Channel], e.User)
	}
	c.mu.Unlock()
	out := []bot.Received{bot.GroupChanged{Chat: ch}}
	if !c.watch.Load() {
		return out
	}
	ev := feature.Event{Chat: ch, Members: &feature.Members{}}
	switch {
	case e.User == c.selfID():
		if !joined {
			return out
		}
		ev.Members.BotAdded = true
		if e.Inviter != "" {
			ev.By = c.person(e.Inviter)
		}
	case joined:
		ev.Members.Joined = []feature.Person{c.person(e.User)}
		if e.Inviter != "" && e.Inviter != e.User {
			ev.By = c.person(e.Inviter)
		}
	default:
		ev.Members.Left = []feature.Person{c.person(e.User)}
	}
	return append(out, bot.Happened{Event: ev})
}

// pressed is a press as the core gets it. Slack wants it acknowledged
// within 3 seconds, which the envelope's acknowledgement did; Answer's
// text, if any, is shown to the presser alone.
func (c *Client) pressed(a *action) []bot.Received {
	if a.User.ID == "" || a.User.ID == c.selfID() || len(a.Actions) == 0 || a.Actions[0].Value == "" {
		return nil
	}
	msgTS, thread := a.Container.MessageTS, a.Container.ThreadTS
	if a.Message != nil {
		msgTS, thread = cmp.Or(a.Message.TS, msgTS), cmp.Or(a.Message.ThreadTS, thread)
	}
	ch := c.chatIn(a.Channel.ID, "", thread, msgTS, a.User.ID)
	if ch == (feature.Chat{}) || msgTS == "" {
		return nil
	}
	if c.nameOf(a.User.ID) == "" {
		c.learnName(a.User.ID, a.User.Name) // their handle, until their name is known
	}
	if thread == msgTS {
		thread = ""
	}
	channel, user := a.Channel.ID, a.User.ID
	answer := func(text string) {
		if text = strings.TrimSpace(text); text != "" {
			go c.ephemeral(channel, thread, user, text)
		}
	}
	return []bot.Received{bot.Pressed{Press: feature.Press{Chat: ch, By: c.person(user), Sent: tsInt(msgTS), Data: a.Actions[0].Value}, Answer: answer}}
}

// command is a slash command as the core gets it: a message "/name args"
// addressed to the bot, in the channel it was used in. In a DM it is the
// user's DM with the bot, whichever DM it was typed in.
func (c *Client) command(s *slash) []bot.Received {
	name := strings.TrimPrefix(s.Command, "/")
	if name == "" || s.UserID == "" || s.ChannelID == "" || s.UserID == c.selfID() {
		return nil
	}
	ch := feature.Chat{GroupID: prefix + s.ChannelID}
	if strings.HasPrefix(s.ChannelID, "D") {
		ch = feature.Chat{User: prefix + s.UserID}
	} else {
		c.seen(s.ChannelID, s.UserID)
	}
	text, _ := c.plain(strings.TrimSpace("/" + strings.ToLower(name) + " " + s.Text))
	user := s.UserID
	return []bot.Received{bot.Incoming{Chat: ch, Load: func(ctx context.Context, learn func(name, id string)) (feature.Message, bool) {
		c.names(ctx, []string{user})
		n := c.nameOf(user)
		if ch.GroupID != "" && n != "" {
			learn(n, prefix+user)
		}
		return feature.Message{Chat: ch, Sender: prefix + user, Name: n, Text: text, Addressed: true}, true
	}}}
}

func (c *Client) person(user string) feature.Person {
	if user == "" {
		return feature.Person{}
	}
	return feature.Person{ID: prefix + user, Name: c.nameOf(user)}
}

// nameOf is a user's display name, as last seen, or "".
func (c *Client) nameOf(user string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.people[user]
}

// learnName remembers a user's display name, made safe.
func (c *Client) learnName(user, name string) {
	name = txt.SafeName(name)
	if user == "" || name == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.people[user]; ok || len(c.people) < 10000 {
		c.people[user] = name
	}
}

// names fetches the display names of users not yet known, by users.info,
// at most 10 at a time.
func (c *Client) names(ctx context.Context, users []string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	slices.Sort(users)
	for i, u := range slices.Compact(users) {
		if i >= 10 || ctx.Err() != nil {
			return
		}
		if u == "" || u == c.selfID() || c.nameOf(u) != "" {
			continue
		}
		if n, err := c.userInfo(ctx, u); err != nil {
			logx.Debugf("slack users.info failed: %v", err)
		} else {
			c.learnName(u, n)
		}
	}
}

// namesLater fetches the names of users not yet known in the background,
// for a message the reader can't wait on, so later ones show them.
func (c *Client) namesLater(users []string) {
	var ask []string
	c.mu.Lock()
	for _, u := range users {
		if _, ok := c.people[u]; !ok && u != "" && !c.asking[u] && len(c.asking) < 100 {
			c.asking[u] = true
			ask = append(ask, u)
		}
	}
	c.mu.Unlock()
	if len(ask) == 0 {
		return
	}
	go func() {
		c.names(context.Background(), ask)
		c.mu.Lock()
		for _, u := range ask {
			delete(c.asking, u)
		}
		c.mu.Unlock()
	}()
}

// userInfo is a user's display name, by users.info.
func (c *Client) userInfo(ctx context.Context, user string) (string, error) {
	var r struct {
		User struct {
			Name     string `json:"name"`
			RealName string `json:"real_name"`
			Profile  struct {
				DisplayName string `json:"display_name"`
				RealName    string `json:"real_name"`
			} `json:"profile"`
		} `json:"user"`
	}
	if err := c.form(ctx, "users.info", c.token, url.Values{"user": {user}}, &r); err != nil {
		return "", err
	}
	u := r.User
	return txt.SafeName(cmp.Or(u.Profile.DisplayName, u.Profile.RealName, u.RealName, u.Name)), nil
}

// seen notes that user posted in channel, for Groups, Delivered's members
// and Redact.
func (c *Client) seen(channel, user string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	people := c.chans[channel]
	if people == nil {
		if len(c.chans) >= 1000 {
			return
		}
		people = map[string]bool{}
		c.chans[channel] = people
	}
	if len(people) < 1000 {
		people[user] = true
	}
}

// Delivered readies a message a feature delivers as its sender, in its
// slot: its sender's name, the bot's @name taken out, and in a channel the
// people seen posting there as its members, as the bot can't list them.
func (c *Client) Delivered(ctx context.Context, msg feature.Message) (bot.Ready, bool) {
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if c.WaitID(wait) == "" {
		logx.Warnf("delivered message dropped: not connected to Slack after 10s")
		return bot.Ready{}, false
	}
	sender := strings.TrimPrefix(msg.Sender, prefix)
	msg.Name = cmp.Or(msg.Name, c.nameOf(sender))
	var r bot.Ready
	if msg.Chat.GroupID != "" {
		channel, _ := c.channelOf(msg.Chat)
		c.mu.Lock()
		for u := range c.chans[channel] {
			if n := c.people[u]; n != "" {
				r.Members = append(r.Members, bot.Member{Name: n, ID: prefix + u})
			}
		}
		c.mu.Unlock()
	}
	rs := []rune(msg.Text)
	if sp := mdstyle.FindAt(rs, map[string]string{c.handleOf(): "", c.BotName(): ""}); len(sp) > 0 {
		var b strings.Builder
		pos := 0
		for _, s := range sp {
			b.WriteString(string(rs[pos:s.Start]))
			pos = s.End
		}
		b.WriteString(string(rs[pos:]))
		msg.Text, r.Mentioned = strings.Join(strings.Fields(b.String()), " "), true
	}
	r.Message = msg
	return r, true
}

// Reachable reports whether the bot may still post in chat on its own: a
// DM, or a channel it hasn't been removed from.
func (c *Client) Reachable(ctx context.Context, chat feature.Chat) bool {
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if c.WaitID(wait) == "" {
		logx.Warnf("not connected to Slack after 10s")
		return false
	}
	if chat.GroupID == "" {
		return true
	}
	channel, _ := c.channelOf(chat)
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.left[channel]
}

// Groups lists the channels where user has been seen since the bot
// started, named by their IDs: the bot's scopes don't let it read a
// channel's name.
func (c *Client) Groups(ctx context.Context, user string) ([]feature.Group, error) {
	user = strings.TrimPrefix(user, prefix)
	c.mu.Lock()
	var out []feature.Group
	for ch, people := range c.chans {
		if people[user] && !c.left[ch] {
			out = append(out, feature.Group{ID: prefix + ch, Name: ch})
		}
	}
	c.mu.Unlock()
	slices.SortFunc(out, func(a, b feature.Group) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// slackID is a Slack user, channel, team or file ID.
var slackID = regexp.MustCompile(`\b[UWCDGTBF][A-Z0-9]{8,}\b`)

// Redact replaces each name the bot has seen with "redacted", as a whole
// word ignoring capitals, longest first, and every Slack ID. It is for
// text the bot logs but doesn't control.
func (c *Client) Redact(s string) string {
	c.mu.Lock()
	var names []string
	for _, n := range c.people {
		names = append(names, strings.Fields(n)...)
	}
	c.mu.Unlock()
	slices.SortFunc(names, func(a, b string) int { return len(b) - len(a) })
	for _, n := range names {
		if len([]rune(n)) >= 2 {
			s = redactWord(s, n)
		}
	}
	return slackID.ReplaceAllString(s, "redacted")
}

// redactWord replaces each whole-word w in s, ignoring capitals.
func redactWord(s, w string) string {
	var b strings.Builder
	rs, ws := []rune(s), []rune(w)
	for i := 0; i < len(rs); {
		if i+len(ws) <= len(rs) && strings.EqualFold(string(rs[i:i+len(ws)]), w) &&
			(i == 0 || !mdstyle.WordRune(rs[i-1])) && (i+len(ws) == len(rs) || !mdstyle.WordRune(rs[i+len(ws)])) {
			b.WriteString("redacted")
			i += len(ws)
			continue
		}
		b.WriteRune(rs[i])
		i++
	}
	return b.String()
}

// Media fetches msg.Media[i] by its file ID: files.info, then a download
// with the bot token from Slack's own file host, refusing one over the
// bot's cap, waiting at most 30s.
func (c *Client) Media(ctx context.Context, msg feature.Message, i int) ([]byte, error) {
	if i < 0 || i >= len(msg.Media) || msg.Media[i].ID == "" {
		return nil, errors.New("no such media")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var r struct {
		File struct {
			URL      string `json:"url_private"`
			Download string `json:"url_private_download"`
			Size     int    `json:"size"`
		} `json:"file"`
	}
	if err := c.form(ctx, "files.info", c.token, url.Values{"file": {msg.Media[i].ID}}, &r); err != nil {
		return nil, err
	}
	if r.File.Size > img.MaxReceived {
		return nil, fmt.Errorf("file too big (%d bytes)", r.File.Size)
	}
	u, err := url.Parse(cmp.Or(r.File.Download, r.File.URL))
	base, _ := url.Parse(c.base)
	if err != nil || base == nil || u.Host != "files.slack.com" && u.Host != base.Host {
		return nil, errors.New("the file is not on Slack's file host") // never send the token elsewhere
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, c.scrub(err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("download failed") // the error quotes the URL
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download: status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, img.MaxReceived+1))
	if err != nil {
		return nil, errors.New("download failed")
	}
	if len(data) > img.MaxReceived {
		return nil, errors.New("file too big")
	}
	return data, nil
}

// sentByBot reports whether the bot sent the message, among those it sent
// lately.
func (c *Client) sentByBot(channel, ts string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sent[channel+"/"+ts]
}

// noteSent remembers a message the bot sent, at most the last 2000.
func (c *Client) noteSent(channel, ts string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := channel + "/" + ts
	c.sent[k] = true
	if c.sentQ = append(c.sentQ, k); len(c.sentQ) > 2000 {
		delete(c.sent, c.sentQ[0])
		c.sentQ = c.sentQ[1:]
	}
}

// noteThread remembers that the message ts in channel is in thread, at most
// the last 2000, for reactions, which name only the channel and ts.
func (c *Client) noteThread(channel, ts, thread string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := channel + "/" + ts
	if _, ok := c.thread[k]; ok {
		return
	}
	c.thread[k] = thread
	if c.threadQ = append(c.threadQ, k); len(c.threadQ) > 2000 {
		delete(c.thread, c.threadQ[0])
		c.threadQ = c.threadQ[1:]
	}
}

// threadOf is the thread the message ts in channel is in, if the bot
// remembers it in one, or "".
func (c *Client) threadOf(channel, ts string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.thread[channel+"/"+ts]
}

// emojiNames are the reactions the bot reads and makes, by Slack's names;
// a reaction not here is skipped, and an emoji not here is sent as a
// message.
var emojiNames = map[string]string{
	"+1": "👍", "-1": "👎", "heart": "❤️", "joy": "😂", "smile": "😄", "slightly_smiling_face": "🙂", "grinning": "😀",
	"heart_eyes": "😍", "tada": "🎉", "fire": "🔥", "eyes": "👀", "white_check_mark": "✅", "x": "❌", "pray": "🙏",
	"clap": "👏", "rocket": "🚀", "thinking_face": "🤔", "cry": "😢", "sob": "😭", "wave": "👋", "ok_hand": "👌",
	"100": "💯", "raised_hands": "🙌", "muscle": "💪", "star": "⭐", "sunglasses": "😎", "open_mouth": "😮",
	"rage": "😡", "warning": "⚠️", "question": "❓", "exclamation": "❗", "sparkles": "✨", "wink": "😉",
	"partying_face": "🥳", "skull": "💀", "see_no_evil": "🙈", "point_up": "☝️", "zap": "⚡", "sunny": "☀️",
	"umbrella": "☔", "snowflake": "❄️", "rainbow": "🌈", "coffee": "☕", "beers": "🍻", "pizza": "🍕", "moneybag": "💰",
	"hourglass": "⌛", "bulb": "💡", "memo": "📝",
}

// emojiAliases are other names Slack sends for the same emoji.
var emojiAliases = map[string]string{"thumbsup": "+1", "thumbsdown": "-1", "laughing": "smile", "satisfied": "smile"}

// emojiOf is the emoji a reaction's name stands for, without any skin tone,
// or "".
func emojiOf(name string) string {
	name, _, _ = strings.Cut(name, "::") // "+1::skin-tone-2"
	return emojiNames[cmp.Or(emojiAliases[name], name)]
}

// nameOfEmoji is emoji's name, ignoring variation selectors, or "".
func nameOfEmoji(emoji string) string {
	bare := strings.ReplaceAll(emoji, "️", "")
	for n, e := range emojiNames {
		if strings.ReplaceAll(e, "️", "") == bare {
			return n
		}
	}
	return ""
}

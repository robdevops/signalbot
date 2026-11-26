package telegram

import (
	"cmp"
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/img"
	"github.com/robdevops/signalbot/internal/kit/mdstyle"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// update is one getUpdates result: the parts the bot reads.
type update struct {
	UpdateID        int64          `json:"update_id"`
	Message         *message       `json:"message"`
	EditedMessage   *message       `json:"edited_message"`
	MessageReaction *reactionUpd   `json:"message_reaction"`
	MyChatMember    *memberUpd     `json:"my_chat_member"`
	ChatMember      *memberUpd     `json:"chat_member"`
	CallbackQuery   *callbackQuery `json:"callback_query"`
}

type user struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
	Username  string `json:"username"`
}

type chat struct {
	ID    int64  `json:"id"`
	Type  string `json:"type"` // private, group, supergroup or channel
	Title string `json:"title"`
}

type entity struct {
	Type   string `json:"type"`
	Offset int    `json:"offset"` // UTF-16 units, as Length
	Length int    `json:"length"`
	User   *user  `json:"user"` // a text_mention's
}

type photoSize struct {
	FileID   string `json:"file_id"`
	FileSize int    `json:"file_size"`
}

type file struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	FileSize int    `json:"file_size"`
}

// animation is a GIF, which Telegram usually turns into a silent MP4; its
// thumbnail is a still the model can see.
type animation struct {
	FileID    string     `json:"file_id"`
	MimeType  string     `json:"mime_type"`
	FileSize  int        `json:"file_size"`
	Thumbnail *photoSize `json:"thumbnail"`
}

type sticker struct {
	FileID     string     `json:"file_id"`
	Emoji      string     `json:"emoji"`
	SetName    string     `json:"set_name"`
	IsAnimated bool       `json:"is_animated"`
	IsVideo    bool       `json:"is_video"`
	Thumbnail  *photoSize `json:"thumbnail"`
}

// video is a video, or a video note, a round one with no name or type;
// its thumbnail is a still the model can see.
type video struct {
	FileID    string     `json:"file_id"`
	FileName  string     `json:"file_name"`
	MimeType  string     `json:"mime_type"`
	FileSize  int        `json:"file_size"`
	Thumbnail *photoSize `json:"thumbnail"`
}

// location is a point on a map; LivePeriod is set on a live location, one
// that follows its sender as they move.
type location struct {
	Latitude   float64 `json:"latitude"`
	Longitude  float64 `json:"longitude"`
	LivePeriod int     `json:"live_period"`
}

// venue is a named place: only its location is read.
type venue struct {
	Location location `json:"location"`
}

type message struct {
	MessageID       int64                  `json:"message_id"`
	ThreadID        int64                  `json:"message_thread_id"`
	IsTopic         bool                   `json:"is_topic_message"`
	TopicCreated    *struct{}              `json:"forum_topic_created"`
	MigrateTo       int64                  `json:"migrate_to_chat_id"`
	MigrateFrom     int64                  `json:"migrate_from_chat_id"`
	From            *user                  `json:"from"`
	Chat            chat                   `json:"chat"`
	Date            int64                  `json:"date"`
	EditDate        int64                  `json:"edit_date"`
	Text            string                 `json:"text"`
	Caption         string                 `json:"caption"`
	Entities        []entity               `json:"entities"`
	CaptionEntities []entity               `json:"caption_entities"`
	ReplyTo         *message               `json:"reply_to_message"`
	Quote           *struct{ Text string } `json:"quote"`
	Photo           []photoSize            `json:"photo"`
	Document        *file                  `json:"document"`
	Animation       *animation             `json:"animation"` // a GIF; Document holds it too, for old clients
	Video           *video                 `json:"video"`
	VideoNote       *video                 `json:"video_note"`
	Audio           *file                  `json:"audio"`
	Voice           *file                  `json:"voice"`
	Sticker         *sticker               `json:"sticker"`
	Location        *location              `json:"location"`
	Venue           *venue                 `json:"venue"`
	NewMembers      []user                 `json:"new_chat_members"`
	LeftMember      *user                  `json:"left_chat_member"`
	TimerChanged    *struct {
		Time int `json:"message_auto_delete_time"`
	} `json:"message_auto_delete_timer_changed"`
}

type reactionUpd struct {
	Chat      chat  `json:"chat"`
	MessageID int64 `json:"message_id"`
	User      *user `json:"user"`
	Old       []struct {
		Type  string `json:"type"`
		Emoji string `json:"emoji"`
	} `json:"old_reaction"`
	New []struct {
		Type  string `json:"type"`
		Emoji string `json:"emoji"`
	} `json:"new_reaction"`
}

type memberUpd struct {
	Chat chat `json:"chat"`
	From user `json:"from"`
	Old  struct {
		Status   string `json:"status"`
		IsMember bool   `json:"is_member"` // a restricted member's
	} `json:"old_chat_member"`
	New struct {
		Status   string `json:"status"`
		IsMember bool   `json:"is_member"`
		User     user   `json:"user"`
	} `json:"new_chat_member"`
}

func id(n int64) string { return strconv.FormatInt(n, 10) }

// in reports whether a member's status is in the chat: a restricted
// member's only if is_member says so, as a restriction outlasts leaving.
func in(status string, isMember bool) bool {
	return status == "creator" || status == "administrator" || status == "member" || status == "restricted" && isMember
}

// chatOf is the chat m was posted in: its group, or a DM with its sender. A
// forum's topic is a chat of its own, "group/topic", as it is a
// conversation of its own.
func chatOf(m *message) feature.Chat {
	if m.Chat.Type == "private" {
		return feature.Chat{User: id(m.Chat.ID)}
	}
	if m.IsTopic && m.ThreadID != 0 {
		return feature.Chat{GroupID: id(m.Chat.ID) + "/" + id(m.ThreadID)}
	}
	return feature.Chat{GroupID: id(m.Chat.ID)}
}

// nameOf is a person's display name, made safe.
func nameOf(u *user) string {
	if u == nil {
		return ""
	}
	return txt.SafeName(strings.TrimSpace(u.FirstName + " " + u.LastName))
}

// events is u as the core gets it: event's, and a group's change for each
// of its topics too, as each is a chat of its own.
func (c *Client) events(u update) []bot.Received {
	r, ok := c.event(u)
	if !ok {
		return nil
	}
	out := []bot.Received{r}
	if gc, isGC := r.(bot.GroupChanged); isGC && !strings.Contains(gc.Chat.GroupID, "/") {
		c.mu.Lock()
		for t := range c.topics[gc.Chat.GroupID] {
			out = append(out, bot.GroupChanged{Chat: feature.Chat{GroupID: t}})
		}
		c.mu.Unlock()
	}
	return out
}

// event is u as the core gets it, decided without waiting on the network;
// false if the core has nothing to do with it.
func (c *Client) event(u update) (bot.Received, bool) {
	c.learn(u)
	switch {
	case u.Message != nil:
		return c.received(u.Message, 0)
	case u.EditedMessage != nil:
		return c.received(u.EditedMessage, cmp.Or(u.EditedMessage.EditDate, u.EditedMessage.Date))
	case u.MyChatMember != nil:
		return c.botMembership(u.MyChatMember)
	case u.ChatMember != nil:
		c.adminChanged(u.ChatMember)
		if !c.watch.Load() {
			return nil, false
		}
		return c.membership(u.ChatMember)
	case u.MessageReaction != nil:
		log.Debugf("a reaction received")
		if !c.watch.Load() {
			return nil, false
		}
		return c.reacted(u.MessageReaction)
	case u.CallbackQuery != nil:
		return c.pressed(u.CallbackQuery)
	}
	return nil, false
}

// learn notes, in the roster, the group an update comes from, but for the
// bot's own membership, which botMembership reads. A message that moves a
// group to a supergroup moves it, from either side; one saying the bot
// left drops it, whether it comes before the bot's my_chat_member or after.
func (c *Client) learn(u update) {
	var ch *chat
	switch {
	case u.Message != nil:
		m := u.Message
		switch {
		case m.MigrateTo != 0: // the old group's last word
			c.migrated(id(m.Chat.ID), id(m.MigrateTo))
			return
		case m.MigrateFrom != 0: // the new supergroup's first
			c.migrated(id(m.MigrateFrom), id(m.Chat.ID))
		case m.LeftMember != nil && id(m.LeftMember.ID) == c.selfID():
			c.dropGroup(id(m.Chat.ID))
			return
		}
		ch = &m.Chat
	case u.EditedMessage != nil:
		ch = &u.EditedMessage.Chat
	case u.CallbackQuery != nil && u.CallbackQuery.Message != nil:
		ch = &u.CallbackQuery.Message.Chat
	case u.MessageReaction != nil:
		ch = &u.MessageReaction.Chat
	case u.ChatMember != nil:
		ch = &u.ChatMember.Chat
	}
	if ch != nil {
		c.learnGroup(*ch)
	}
}

// received is a message, or with edited an edit of one, as the core gets
// it. A DM from someone TELEGRAM_DM_USERS doesn't allow is refused here
// when that is known without asking Telegram, else in its slot (load); an
// edit refused here forgets the original and keeps nothing (the Bot API
// never reports deletes, but edits it does).
func (c *Client) received(m *message, edited int64) (bot.Received, bool) {
	if m.From == nil || m.Chat.Type == "channel" {
		return nil, false
	}
	ch, self := chatOf(m), c.selfID()
	if ch.GroupID != "" {
		if m.MigrateTo != 0 { // the group became a supergroup, with a new ID
			c.migrated(id(m.Chat.ID), id(m.MigrateTo))
			return bot.GroupChanged{Chat: ch}, true
		}
		c.seen(id(m.Chat.ID), ch.GroupID, m)
		c.checkStatus(id(m.Chat.ID))
	}
	if len(m.NewMembers) > 0 || m.LeftMember != nil {
		return bot.GroupChanged{Chat: feature.Chat{GroupID: id(m.Chat.ID)}}, true // forget the names it links to
	}
	if self == "" || id(m.From.ID) == self {
		return nil, false
	}
	if ch.GroupID == "" {
		if ok, known := c.dmKnown(id(m.From.ID)); known && !ok {
			if edited != 0 {
				return bot.Deleted{Chat: ch, Sender: id(m.From.ID), Sent: m.MessageID, At: edited}, true
			}
			c.refused.Log(id(m.From.ID))
			return nil, false
		}
	}
	if t := m.TimerChanged; t != nil {
		c.mu.Lock()
		if _, ok := c.timers[id(m.Chat.ID)]; ok || len(c.timers) < 10000 {
			c.timers[id(m.Chat.ID)] = time.Duration(t.Time) * time.Second
		}
		c.mu.Unlock()
	}
	msg := c.message(m, edited)
	switch {
	case msg.Text == "" && len(msg.Media) == 0:
		return nil, false
	case msg.Addressed:
		return bot.Incoming{Chat: ch, Load: func(ctx context.Context, learn func(name, id string)) (feature.Message, bool) {
			return c.load(ctx, m, msg, learn)
		}}, true
	case ch.GroupID != "":
		return bot.Chatter{Message: msg}, true
	}
	return nil, false
}

// load completes an addressed message in its slot: in a DM, only from
// someone TELEGRAM_DM_USERS allows, asking Telegram whether they are an
// admin if need be; the chat's auto-delete timer; and the names it shows
// the model, for @Name in the reply.
func (c *Client) load(ctx context.Context, m *message, msg feature.Message, learn func(name, id string)) (feature.Message, bool) {
	if msg.Chat.GroupID == "" && !c.dmAllowed(ctx, msg.Sender) {
		c.refused.Log(msg.Sender)
		return msg, false
	}
	msg.Expires = c.timer(ctx, m.Chat.ID)
	if msg.Chat.GroupID != "" {
		if n := nameOf(m.From); n != "" {
			learn(n, msg.Sender)
		}
		for _, e := range append(slices.Clone(m.Entities), m.CaptionEntities...) {
			if e.Type == "text_mention" && e.User != nil && id(e.User.ID) != c.selfID() {
				if n := nameOf(e.User); n != "" {
					learn(n, id(e.User.ID))
				}
			}
		}
	}
	return msg, true
}

// timer is the chat's auto-delete timer, asked of getChat once, then kept
// up to date from the messages that change it.
func (c *Client) timer(ctx context.Context, chatID int64) time.Duration {
	c.mu.Lock()
	t, ok := c.timers[id(chatID)]
	c.mu.Unlock()
	if ok {
		return t
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var full struct {
		Time int `json:"message_auto_delete_time"`
	}
	if err := c.call(ctx, "getChat", map[string]int64{"chat_id": chatID}, &full); err != nil {
		log.Warnf("getChat failed: %v", err)
		return 0
	}
	t = time.Duration(full.Time) * time.Second
	c.mu.Lock()
	if len(c.timers) < 10000 {
		c.timers[id(chatID)] = t
	}
	c.mu.Unlock()
	return t
}

// message is m as the core and features see it, with edited when it is an
// edit: its text with the bot's own @mention and /command@bot suffix taken
// out, other people's text mentions as @Name, and its media.
func (c *Client) message(m *message, edited int64) feature.Message {
	text, ents := m.Text, m.Entities
	if text == "" {
		text, ents = m.Caption, m.CaptionEntities
	}
	ch := chatOf(m)
	body, mentioned := c.text(text, ents)
	if link := mapLink(m); link != "" {
		body = strings.TrimSpace(link + "\n" + body)
	}
	msg := feature.Message{Chat: ch, Sender: id(m.From.ID), Name: nameOf(m.From), Text: body, Timestamp: m.MessageID, Edited: edited}
	self := c.selfID()
	replyToBot := m.ReplyTo != nil && m.ReplyTo.From != nil && id(m.ReplyTo.From.ID) == self
	msg.Addressed = ch.GroupID == "" || mentioned || replyToBot
	var images, files []feature.Media
	add := func(m *message, quoted bool) {
		if m == nil {
			return
		}
		if p := best(m.Photo); p != nil && len(images) < feature.MaxPictures {
			images = append(images, feature.Media{ID: p.FileID, Quoted: quoted})
		}
		doc := m.Document
		if a := m.Animation; a != nil {
			doc = nil // the same GIF, not a file to read
			still := a.Thumbnail
			if img.Supported(a.MimeType) && a.FileSize <= img.MaxReceived { // a GIF kept as one: itself
				still = &photoSize{FileID: a.FileID, FileSize: a.FileSize}
			}
			if still != nil && still.FileSize <= img.MaxReceived && len(images) < feature.MaxPictures {
				images = append(images, feature.Media{ID: still.FileID, Quoted: quoted})
			}
		}
		fs := []*file{doc}
		for _, v := range []*video{m.Video, m.VideoNote} {
			if v == nil {
				continue
			}
			if t := v.Thumbnail; t != nil && t.FileSize <= img.MaxReceived && len(images) < feature.MaxPictures {
				images = append(images, feature.Media{ID: t.FileID, Quoted: quoted})
			}
			fs = append(fs, &file{FileID: v.FileID, FileName: v.FileName, MimeType: cmp.Or(v.MimeType, "video/mp4"), FileSize: v.FileSize})
		}
		for _, f := range append(fs, m.Audio, m.Voice) {
			switch {
			case f == nil:
			case img.Supported(f.MimeType) && f.FileSize <= img.MaxReceived && len(images) < feature.MaxPictures:
				images = append(images, feature.Media{ID: f.FileID, Quoted: quoted})
			case len(files) < feature.MaxFiles:
				fid := f.FileID
				if f.FileSize > img.MaxReceived {
					fid = "" // too big to fetch: noted, not fetched
				}
				files = append(files, feature.Media{ID: fid, Quoted: quoted, File: true, Name: f.FileName, MIME: f.MimeType, Size: f.FileSize})
			}
		}
	}
	add(m, false)
	if r := m.ReplyTo; r != nil && r.TopicCreated == nil { // in a topic, every message replies to the one that made it

		qt := cmp.Or(r.Text, r.Caption)
		if m.Quote != nil && m.Quote.Text != "" {
			qt = m.Quote.Text // the part quoted
		}
		msg.Quote = &feature.Quote{Text: qt, Author: nameOf(r.From), Bot: r.From != nil && id(r.From.ID) == self}
		if r.From != nil {
			msg.Quote.Sender, msg.Quote.Sent = id(r.From.ID), r.MessageID
		}
		add(r, true)
	}
	msg.Media = slices.Concat(images, files)
	if s := m.Sticker; s != nil {
		fid := s.FileID
		if s.IsAnimated || s.IsVideo { // only a static WebP can be shown: its thumbnail stands in
			fid = ""
			if s.Thumbnail != nil {
				fid = s.Thumbnail.FileID
			}
		}
		if fid != "" {
			label := ""
			if e := txt.Clamp(txt.SafeName(s.Emoji), 8); e != "" {
				label = ": " + e
			}
			if n := txt.SafeName(s.SetName); n != "" {
				label += " from " + strconv.Quote(n)
			}
			msg.Media = append(msg.Media, feature.Media{ID: fid, Sticker: true, Label: label})
		}
	}
	return msg
}

// mapLink is m's location or venue as a Google Maps link, which is how
// Signal sends one, or "" if it has none. A live location is left out: it
// is where its sender is right now.
func mapLink(m *message) string {
	l := m.Location
	if l == nil && m.Venue != nil {
		l = &m.Venue.Location
	}
	if l == nil || l.LivePeriod != 0 {
		return ""
	}
	return "https://maps.google.com/maps?q=" + strconv.FormatFloat(l.Latitude, 'f', -1, 64) + "," + strconv.FormatFloat(l.Longitude, 'f', -1, 64)
}

// best is the largest size of a photo within the bot's cap, or nil.
func best(sizes []photoSize) *photoSize {
	var b *photoSize
	for i := range sizes {
		if s := &sizes[i]; s.FileSize <= img.MaxReceived && (b == nil || s.FileSize >= b.FileSize) {
			b = s
		}
	}
	return b
}

// text is a message's text with the bot's @username or text mention taken
// out, a command's @username suffix too, and other text mentions written
// @Name; mentioned says the bot was addressed so, or by a command at the
// start without another bot's name.
func (c *Client) text(s string, ents []entity) (string, bool) {
	units := utf16.Encode([]rune(s))
	self, me := c.selfID(), "@"+strings.ToLower(c.usernameOf())
	var b strings.Builder
	pos, mentioned := 0, false
	slices.SortStableFunc(ents, func(a, b entity) int { return a.Offset - b.Offset })
	for _, e := range ents {
		if e.Offset < pos || e.Length <= 0 || e.Offset+e.Length > len(units) {
			continue // overlapping or out of range: left as written
		}
		part := string(utf16.Decode(units[e.Offset : e.Offset+e.Length]))
		var repl string
		switch {
		case e.Type == "mention" && strings.ToLower(part) == me:
			mentioned = true
		case e.Type == "text_mention" && e.User != nil && id(e.User.ID) == self:
			mentioned = true
		case e.Type == "text_mention" && e.User != nil:
			repl = "@" + cmp.Or(nameOf(e.User), "[unnamed user]")
		case e.Type == "bot_command" && e.Offset == 0:
			cmd, at, has := strings.Cut(part, "@")
			if has && "@"+strings.ToLower(at) != me {
				continue // another bot's
			}
			mentioned, repl = true, cmd
		default:
			continue
		}
		b.WriteString(string(utf16.Decode(units[pos:e.Offset])))
		b.WriteString(repl)
		pos = e.Offset + e.Length
	}
	b.WriteString(string(utf16.Decode(units[pos:])))
	return strings.TrimSpace(strings.Join(strings.FieldsFunc(b.String(), func(r rune) bool { return r == ' ' }), " ")), mentioned
}

func (c *Client) usernameOf() string { u, _ := c.username.Load().(string); return u }

// maxGroups caps the groups the client remembers.
const maxGroups = 1000

// seen notes a group, its title, the topic chat the message is in, and who
// posted there, for Groups, Delivered's members and Redact.
func (c *Client) seen(group, chatGroup string, m *message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.groups[group]; !ok {
		if len(c.groups) >= maxGroups {
			return
		}
		c.forgetNoes() // someone refused may be an admin here
	}
	c.groups[group] = txt.SafeName(m.Chat.Title)
	if chatGroup != group {
		ts := c.topics[group]
		if ts == nil {
			ts = map[string]bool{}
			c.topics[group] = ts
		}
		if len(ts) < 200 {
			ts[chatGroup] = true
		}
	}
	people := c.chats[group]
	if people == nil {
		people = map[string]string{}
		c.chats[group] = people
	}
	if n := nameOf(m.From); n != "" && len(people) < 1000 && len(c.people) < 10000 {
		people[id(m.From.ID)], c.people[id(m.From.ID)] = n, n
	}
}

// migrated moves what the client knows of a group to its new ID, after it
// became a supergroup.
func (c *Client) migrated(from, to string) {
	c.mu.Lock()
	changed := c.moveRoster(from, to)
	defer func() {
		c.mu.Unlock()
		if changed {
			c.saveRoster()
		}
	}()
	c.moved[from] = to
	delete(c.topics, from) // a basic group has none
	delete(c.admins, from)
	if t, ok := c.groups[from]; ok {
		c.groups[to] = t
		delete(c.groups, from)
	}
	if p, ok := c.chats[from]; ok {
		c.chats[to] = p
		delete(c.chats, from)
	}
}

// botMembership is the bot added to a group or removed from one: it posts
// in a group while it is in it. Who may DM the bot is asked again, as
// someone may be an admin only of that group.
func (c *Client) botMembership(u *memberUpd) (bot.Received, bool) {
	if u.Chat.Type == "private" || u.Chat.Type == "channel" {
		return nil, false
	}
	g := id(u.Chat.ID)
	now := in(u.New.Status, u.New.IsMember)
	added := now && !in(u.Old.Status, u.Old.IsMember)
	c.mu.Lock()
	delete(c.admins, g)
	delete(c.status, g) // asked again: the bot may have been made an admin, or stopped being one
	clear(c.dmOK)
	c.forgot++
	if now {
		delete(c.left, g)
		if _, ok := c.groups[g]; ok || len(c.groups) < maxGroups {
			c.groups[g] = txt.SafeName(u.Chat.Title)
		}
	} else {
		if len(c.left) < 2*maxGroups {
			c.left[g] = true
		}
		delete(c.groups, g)
		delete(c.chats, g)
	}
	c.mu.Unlock()
	if now {
		c.learnGroup(u.Chat)
	} else {
		c.dropGroup(g)
	}
	if added && c.watch.Load() {
		return bot.Happened{Event: feature.Event{Chat: feature.Chat{GroupID: g}, By: c.person(&u.From), Members: &feature.Members{BotAdded: true}}}, true
	}
	return bot.GroupChanged{Chat: feature.Chat{GroupID: g}}, true
}

// membership is someone else joining or leaving a group; Telegram sends it
// only when the bot is an admin there.
func (c *Client) membership(u *memberUpd) (bot.Received, bool) {
	if u.Chat.Type == "private" || u.Chat.Type == "channel" || id(u.New.User.ID) == c.selfID() {
		return nil, false
	}
	p, ms := c.person(&u.New.User), &feature.Members{}
	was, is := in(u.Old.Status, u.Old.IsMember), in(u.New.Status, u.New.IsMember)
	switch {
	case is && !was:
		ms.Joined = []feature.Person{p}
	case !is && was:
		ms.Left = []feature.Person{p}
	default:
		return nil, false
	}
	by := c.person(&u.From)
	if by.ID == p.ID {
		by = feature.Person{}
	}
	return bot.Happened{Event: feature.Event{Chat: feature.Chat{GroupID: id(u.Chat.ID)}, By: by, Members: ms}}, true
}

// reacted is a reaction as the core gets it, but the bot's own, and in a DM
// only from someone listed in TELEGRAM_DM_USERS or an admin by a
// verdict from the last 10 minutes, as the reader can't ask Telegram.
// Telegram sends reactions in a group only when the bot is an admin there.
func (c *Client) reacted(u *reactionUpd) (bot.Received, bool) {
	if u.User == nil || id(u.User.ID) == c.selfID() {
		return nil, false
	}
	byBot, topic := c.sentByBot(id(u.Chat.ID), u.MessageID)
	ch := feature.Chat{GroupID: cmp.Or(topic, id(u.Chat.ID))} // a reaction doesn't say which topic: the bot's own message's is known
	if u.Chat.Type == "private" {
		if ch = (feature.Chat{User: id(u.Chat.ID)}); !c.dmFresh(id(u.User.ID)) {
			return nil, false
		}
	}
	emoji, removed := "", false
	for _, r := range u.New {
		if r.Type == "emoji" {
			emoji = r.Emoji
		}
	}
	if emoji == "" {
		for _, r := range u.Old {
			if r.Type == "emoji" {
				emoji, removed = r.Emoji, true
			}
		}
	}
	if emoji == "" {
		return nil, false
	}
	author := ""
	if byBot {
		author = c.selfID()
	}
	return bot.Happened{Event: feature.Event{Chat: ch, By: c.person(u.User), Reaction: &feature.Reaction{
		Emoji: txt.Clamp(emoji, 16), Removed: removed, Author: author, Bot: byBot, Sent: u.MessageID}}}, true
}

func (c *Client) person(u *user) feature.Person {
	if u == nil || u.ID == 0 {
		return feature.Person{}
	}
	return feature.Person{ID: id(u.ID), Name: nameOf(u)}
}

// Delivered readies a message a feature delivers as its sender, in its
// slot: in a DM, only while TELEGRAM_DM_USERS allows its sender
// (asking Telegram whether they are an admin if need be); in a group,
// only while its sender is there. Its members are the group's admins and
// the people seen posting there, as the Bot API can't list a group's
// members.
func (c *Client) Delivered(ctx context.Context, msg feature.Message) (bot.Ready, bool) {
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if c.WaitID(wait) == "" {
		log.Warnf("delivered message dropped: not connected to Telegram after 10s")
		return bot.Ready{}, false
	}
	if msg.Chat.GroupID == "" && !c.dmAllowed(wait, msg.Sender) {
		log.Infof("delivered message dropped: its sender may no longer DM the bot")
		return bot.Ready{}, false
	}
	c.mu.Lock()
	msg.Name = cmp.Or(msg.Name, c.people[msg.Sender])
	c.mu.Unlock()
	var r bot.Ready
	if msg.Chat.GroupID != "" {
		g := c.dest(msg.Chat).chat
		if !c.member(wait, g, msg.Sender) {
			log.Infof("delivered message dropped: its sender is not in the group")
			return bot.Ready{}, false
		}
		g = c.dest(msg.Chat).chat // at its new ID, if it moved
		admins, err := c.groupAdmins(wait, g)
		if err != nil {
			log.Warnf("getChatAdministrators failed: %v", err)
		}
		seen := map[string]bool{}
		for _, a := range admins {
			if n := nameOf(&a); n != "" && !a.IsBot {
				r.Members, seen[id(a.ID)] = append(r.Members, bot.Member{Name: n, ID: id(a.ID)}), true
			}
		}
		c.mu.Lock()
		for pid, n := range c.chats[g] {
			if !seen[pid] {
				r.Members = append(r.Members, bot.Member{Name: n, ID: pid})
			}
		}
		c.mu.Unlock()
	}
	rs := []rune(msg.Text)
	known := map[string]string{}
	for _, n := range []string{c.usernameOf(), c.BotName()} {
		if n != "" {
			known[n] = ""
		}
	}
	if sp := mdstyle.FindAt(rs, known); len(sp) > 0 {
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

// Reachable reports whether the bot may still post in chat on its own: the
// DM of a user TELEGRAM_DM_USERS still allows (asking Telegram whether
// they are an admin if need be; a failed lookup refuses), or a group it
// hasn't been removed from. (A user who blocked the bot, or a group that
// is gone, refuses the send itself.)
func (c *Client) Reachable(ctx context.Context, chat feature.Chat) bool {
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if c.WaitID(wait) == "" {
		log.Warnf("not connected to Telegram after 10s")
		return false
	}
	if chat.GroupID == "" {
		return c.dmAllowed(wait, chat.User)
	}
	g := c.dest(chat).chat
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.left[g]
}

// member reports whether user is in group, by getChatMember, logging a
// failure.
func (c *Client) member(ctx context.Context, group, user string) bool {
	there, err := c.isMember(ctx, group, user)
	if err != nil {
		log.Warnf("getChatMember failed: %v", err)
	}
	return there
}

// isMember asks getChatMember whether user is in group, at the group's new
// ID if it became a supergroup.
func (c *Client) isMember(ctx context.Context, group, user string) (bool, error) {
	var m struct {
		Status   string `json:"status"`
		IsMember bool   `json:"is_member"`
	}
	params := map[string]string{"chat_id": group, "user_id": user}
	err := c.retrying(ctx, group, func(to string) {
		params["chat_id"] = to
		c.migrated(group, to)
	}, func() error { return c.call(ctx, "getChatMember", params, &m) })
	return err == nil && in(m.Status, m.IsMember), err
}

// Groups lists the groups the bot shares with user, among those it has
// seen since it started: the Bot API can't list a bot's groups. Admin is
// whether they are the group's owner or an administrator
// (getChatAdministrators, kept 10 minutes); Telegram has no mods.
func (c *Client) Groups(ctx context.Context, user string) ([]feature.Group, error) {
	c.mu.Lock()
	type g struct{ id, name string }
	var gs []g
	for k, v := range c.groups {
		gs = append(gs, g{k, v})
	}
	c.mu.Unlock()
	slices.SortFunc(gs, func(a, b g) int { return strings.Compare(a.name, b.name) })
	var out []feature.Group
	for _, x := range gs {
		if !c.member(ctx, x.id, user) {
			continue
		}
		admins, err := c.groupAdmins(ctx, x.id)
		if err != nil {
			log.Warnf("getChatAdministrators failed: %v", err)
		}
		out = append(out, feature.Group{ID: x.id, Name: x.name, Admin: slices.ContainsFunc(admins, isUser(user))})
	}
	return out, nil
}

// telegramID is a Telegram user or chat ID: digits, often long, and a
// group's negative.
var telegramID = regexp.MustCompile(`-?\b\d{6,}\b`)

// Redact replaces each name the bot has seen with "redacted", as a whole
// word ignoring capitals, longest first, and every number long enough to
// be a Telegram ID. It is for text the bot logs but doesn't control.
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
	return telegramID.ReplaceAllString(s, "redacted")
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

// sentByBot reports whether the bot sent the message, among those it sent
// lately, and the topic chat it was sent in ("group/topic"), if any.
func (c *Client) sentByBot(chat string, msgID int64) (ok bool, topic string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	topic, ok = c.sent[chat+"/"+id(msgID)]
	return ok, topic
}

// noteSent remembers a message the bot sent to ch, at chat, its ID there
// (a group's new one, if it moved), and ch's topic, at most the last 2000.
func (c *Client) noteSent(ch feature.Chat, chat string, msgID int64) {
	topic := ""
	if strings.Contains(ch.GroupID, "/") {
		topic = ch.GroupID
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	k := chat + "/" + id(msgID)
	if _, ok := c.sent[k]; ok {
		c.sent[k] = topic
		return
	}
	c.sent[k] = topic
	if c.sentQ = append(c.sentQ, k); len(c.sentQ) > 2000 {
		delete(c.sent, c.sentQ[0])
		c.sentQ = c.sentQ[1:]
	}
}

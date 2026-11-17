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
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/mdstyle"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// update is one getUpdates result: the parts the bot reads.
type update struct {
	UpdateID        int64        `json:"update_id"`
	Message         *message     `json:"message"`
	EditedMessage   *message     `json:"edited_message"`
	MessageReaction *reactionUpd `json:"message_reaction"`
	MyChatMember    *memberUpd   `json:"my_chat_member"`
	ChatMember      *memberUpd   `json:"chat_member"`
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

type sticker struct {
	FileID     string     `json:"file_id"`
	Emoji      string     `json:"emoji"`
	SetName    string     `json:"set_name"`
	IsAnimated bool       `json:"is_animated"`
	IsVideo    bool       `json:"is_video"`
	Thumbnail  *photoSize `json:"thumbnail"`
}

type message struct {
	MessageID       int64                  `json:"message_id"`
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
	Audio           *file                  `json:"audio"`
	Voice           *file                  `json:"voice"`
	Sticker         *sticker               `json:"sticker"`
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
	Chat chat                    `json:"chat"`
	From user                    `json:"from"`
	Old  struct{ Status string } `json:"old_chat_member"`
	New  struct {
		Status string `json:"status"`
		User   user   `json:"user"`
	} `json:"new_chat_member"`
}

func id(n int64) string { return strconv.FormatInt(n, 10) }

// in reports whether a member's status is in the chat.
func in(status string) bool {
	return status == "creator" || status == "administrator" || status == "member" || status == "restricted"
}

// chatOf is the chat m was posted in: its group, or a DM with its sender.
func chatOf(m *message) feature.Chat {
	if m.Chat.Type == "private" {
		return feature.Chat{User: id(m.Chat.ID)}
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

// event is u as the core gets it, decided without calling the network;
// false if the core has nothing to do with it.
func (c *Client) event(u update) (bot.Received, bool) {
	switch {
	case u.Message != nil:
		return c.received(u.Message, 0)
	case u.EditedMessage != nil:
		return c.received(u.EditedMessage, cmp.Or(u.EditedMessage.EditDate, u.EditedMessage.Date))
	case u.MyChatMember != nil:
		return c.botMembership(u.MyChatMember)
	case u.ChatMember != nil && c.watch.Load():
		return c.membership(u.ChatMember)
	case u.MessageReaction != nil && c.watch.Load():
		return c.reacted(u.MessageReaction)
	}
	return nil, false
}

// received is a message, or with edited an edit of one, as the core gets
// it. An edit in the DM of someone the bot doesn't answer, such as someone
// removed from TELEGRAM_DM_USER_IDS since, forgets the original and keeps
// nothing: the Bot API never reports deletes, but edits it does.
func (c *Client) received(m *message, edited int64) (bot.Received, bool) {
	if m.From == nil || m.Chat.Type == "channel" {
		return nil, false
	}
	ch, self := chatOf(m), c.selfID()
	if ch.GroupID != "" {
		c.seen(ch.GroupID, m)
	}
	if t := m.TimerChanged; t != nil {
		c.mu.Lock()
		c.timers[id(m.Chat.ID)] = time.Duration(t.Time) * time.Second
		c.mu.Unlock()
	}
	if len(m.NewMembers) > 0 || m.LeftMember != nil {
		return bot.GroupChanged{Chat: ch}, true // forget the names it links to
	}
	if self == "" || id(m.From.ID) == self {
		return nil, false
	}
	if ch.GroupID == "" && !c.allowed[id(m.From.ID)] {
		if edited != 0 {
			return bot.Deleted{Chat: ch, Sender: id(m.From.ID), Sent: m.MessageID, At: edited}, true
		}
		logx.Infof("%s ignored because not in TELEGRAM_DM_USER_IDS (add this ID to allow DMs)", id(m.From.ID))
		return nil, false
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

// load completes an addressed message in its slot: the chat's auto-delete
// timer, and the names it shows the model, for @Name in the reply.
func (c *Client) load(ctx context.Context, m *message, msg feature.Message, learn func(name, id string)) (feature.Message, bool) {
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
		logx.Warnf("telegram getChat failed: %v", err)
		return 0
	}
	t = time.Duration(full.Time) * time.Second
	c.mu.Lock()
	c.timers[id(chatID)] = t
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
		for _, f := range []*file{m.Document, m.Audio, m.Voice} {
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
	if r := m.ReplyTo; r != nil {
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

// seen notes a group, its title, and who posted there, for Groups,
// Delivered's members and Redact.
func (c *Client) seen(group string, m *message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.groups) < 1000 {
		c.groups[group] = txt.SafeName(m.Chat.Title)
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

// botMembership is the bot added to a group or removed from one.
func (c *Client) botMembership(u *memberUpd) (bot.Received, bool) {
	if u.Chat.Type == "private" || u.Chat.Type == "channel" {
		return nil, false
	}
	g := id(u.Chat.ID)
	added := in(u.New.Status) && !in(u.Old.Status)
	c.mu.Lock()
	if in(u.New.Status) {
		c.groups[g] = txt.SafeName(u.Chat.Title)
	} else {
		delete(c.groups, g)
		delete(c.chats, g)
	}
	c.mu.Unlock()
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
	switch {
	case in(u.New.Status) && !in(u.Old.Status):
		ms.Joined = []feature.Person{p}
	case !in(u.New.Status) && in(u.Old.Status):
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

// reacted is a reaction as the core gets it: not the bot's own, nor in the
// DM of someone the bot doesn't answer. Telegram sends reactions in a
// group only when the bot is an admin there.
func (c *Client) reacted(u *reactionUpd) (bot.Received, bool) {
	if u.User == nil || id(u.User.ID) == c.selfID() {
		return nil, false
	}
	ch := feature.Chat{GroupID: id(u.Chat.ID)}
	if u.Chat.Type == "private" {
		if ch = (feature.Chat{User: id(u.Chat.ID)}); !c.allowed[id(u.User.ID)] {
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
	byBot := c.sentByBot(ch, u.MessageID)
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
// slot: never in the DM of someone the bot doesn't answer, nor in a group
// its sender has left. Its members are the group's admins and the people
// seen posting there, as the Bot API can't list a group's members.
func (c *Client) Delivered(ctx context.Context, msg feature.Message) (bot.Ready, bool) {
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if c.WaitID(wait) == "" {
		logx.Warnf("delivered message dropped: not connected to Telegram after 10s")
		return bot.Ready{}, false
	}
	if msg.Chat.GroupID == "" && !c.allowed[msg.Sender] {
		logx.Infof("delivered message from %s dropped because not in TELEGRAM_DM_USER_IDS (add this ID to allow DMs)", cmp.Or(msg.Sender, "unknown sender"))
		return bot.Ready{}, false
	}
	c.mu.Lock()
	msg.Name = cmp.Or(msg.Name, c.people[msg.Sender])
	c.mu.Unlock()
	var r bot.Ready
	if g := msg.Chat.GroupID; g != "" {
		if !c.member(wait, g, msg.Sender) {
			logx.Infof("delivered message dropped: its sender is not in the group")
			return bot.Ready{}, false
		}
		var admins []struct{ User user }
		if err := c.call(wait, "getChatAdministrators", map[string]string{"chat_id": g}, &admins); err != nil {
			logx.Warnf("telegram getChatAdministrators failed: %v", err)
		}
		seen := map[string]bool{}
		for _, a := range admins {
			if n := nameOf(&a.User); n != "" && !a.User.IsBot {
				r.Members, seen[id(a.User.ID)] = append(r.Members, bot.Member{Name: n, ID: id(a.User.ID)}), true
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

// member reports whether user is in group, by getChatMember.
func (c *Client) member(ctx context.Context, group, user string) bool {
	var m struct {
		Status   string `json:"status"`
		IsMember bool   `json:"is_member"` // a restricted member's
	}
	if err := c.call(ctx, "getChatMember", map[string]string{"chat_id": group, "user_id": user}, &m); err != nil {
		logx.Warnf("telegram getChatMember failed: %v", err)
		return false
	}
	return in(m.Status) && (m.Status != "restricted" || m.IsMember)
}

// Groups lists the groups the bot shares with user, among those it has
// seen since it started: the Bot API can't list a bot's groups.
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
		if c.member(ctx, x.id, user) {
			out = append(out, feature.Group{ID: x.id, Name: x.name})
		}
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
// lately.
func (c *Client) sentByBot(ch feature.Chat, msgID int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sent[ch.Key()+"/"+id(msgID)]
}

// noteSent remembers a message the bot sent, at most the last 2000.
func (c *Client) noteSent(ch feature.Chat, msgID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	k := ch.Key() + "/" + id(msgID)
	c.sent[k] = true
	if c.sentQ = append(c.sentQ, k); len(c.sentQ) > 2000 {
		delete(c.sent, c.sentQ[0])
		c.sentQ = c.sentQ[1:]
	}
}

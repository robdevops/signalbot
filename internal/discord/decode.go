package discord

import (
	"cmp"
	"context"
	"encoding/json"
	"math"
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

type user struct {
	ID         string  `json:"id"`
	Username   string  `json:"username"`
	GlobalName string  `json:"global_name"`
	Bot        bool    `json:"bot"`
	Member     *member `json:"member"` // a mention's, in a guild
}

type member struct {
	Nick string `json:"nick"`
	User *user  `json:"user"`
}

type attachment struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
	URL         string `json:"url"`
}

// embed is a link's preview. A GIF from the GIF picker (gifv, a video with
// a still) or a picture (image) is shown to the model by the still Discord
// keeps on its media proxy.
type embed struct {
	Type      string `json:"type"`
	URL       string `json:"url"`
	Thumbnail *struct {
		ProxyURL string `json:"proxy_url"`
	} `json:"thumbnail"`
}

// picture is the link to e's still on Discord's media proxy, or "" if it
// is no GIF or picture, or has none there.
func (e embed) picture() string {
	if e.Type != "gifv" && e.Type != "image" || e.Thumbnail == nil {
		return ""
	}
	u, err := url.Parse(e.Thumbnail.ProxyURL)
	if err != nil || u.Scheme != "https" || !strings.HasSuffix(u.Hostname(), ".discordapp.net") {
		return ""
	}
	return u.String()
}

type message struct {
	ID          string       `json:"id"`
	ChannelID   string       `json:"channel_id"`
	GuildID     string       `json:"guild_id"`
	Type        int          `json:"type"`
	Author      *user        `json:"author"`
	Member      *member      `json:"member"`
	WebhookID   string       `json:"webhook_id"`
	Content     string       `json:"content"`
	Edited      string       `json:"edited_timestamp"`
	Mentions    []user       `json:"mentions"`
	Roles       []string     `json:"mention_roles"`
	Attachments []attachment `json:"attachments"`
	Embeds      []embed      `json:"embeds"`
	Stickers    []struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Format int    `json:"format_type"`
	} `json:"sticker_items"`
	Reference *struct {
		MessageID string `json:"message_id"`
	} `json:"message_reference"`
	Referenced *message `json:"referenced_message"`
}

// nameOf is a person's display name, made safe: their nickname in the
// guild, else their own.
func nameOf(u *user, m *member) string {
	if u == nil {
		return ""
	}
	nick := ""
	if m != nil {
		nick = m.Nick
	}
	return txt.SafeName(cmp.Or(nick, u.GlobalName, u.Username))
}

// events is a gateway event as the core gets it, decided without waiting
// on the network: none if the core has nothing to do with it.
func (c *Client) events(t string, d json.RawMessage) []bot.Received {
	switch t {
	case "MESSAGE_CREATE", "MESSAGE_UPDATE":
		var m message
		if json.Unmarshal(d, &m) != nil {
			return nil
		}
		var edited int64
		if t == "MESSAGE_UPDATE" {
			e, err := time.Parse(time.RFC3339, m.Edited)
			if err != nil {
				return nil // not an edit of its text, such as a link's preview
			}
			edited = max(at(e), snowflake(m.ID)+1)
		}
		if r := c.received(&m, edited); r != nil {
			return []bot.Received{r}
		}
	case "MESSAGE_DELETE":
		return c.deleted(d)
	case "MESSAGE_REACTION_ADD", "MESSAGE_REACTION_REMOVE":
		if c.watch.Load() {
			return c.reacted(d, t == "MESSAGE_REACTION_REMOVE")
		}
	case "GUILD_CREATE", "GUILD_DELETE":
		return c.guild(d, t == "GUILD_DELETE")
	case "GUILD_ROLE_CREATE", "GUILD_ROLE_UPDATE", "GUILD_ROLE_DELETE":
		c.roleChanged(d, t == "GUILD_ROLE_DELETE")
	case "CHANNEL_CREATE", "THREAD_CREATE":
		var ch struct {
			ID      string `json:"id"`
			GuildID string `json:"guild_id"`
			Name    string `json:"name"`
		}
		if json.Unmarshal(d, &ch) == nil && ch.GuildID != "" {
			c.mu.Lock()
			c.addChannel(ch.ID, channel{guild: ch.GuildID, name: txt.SafeName(ch.Name)})
			c.mu.Unlock()
		}
	case "CHANNEL_DELETE", "THREAD_DELETE":
		var ch struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(d, &ch) == nil && ch.ID != "" {
			c.mu.Lock()
			c.leave(ch.ID)
			c.mu.Unlock()
			return []bot.Received{bot.GroupChanged{Chat: feature.Chat{GroupID: prefix + ch.ID}}}
		}
	case "INTERACTION_CREATE":
		if r := c.interaction(d); r != nil {
			return []bot.Received{r}
		}
	}
	return nil
}

// received is a message, or with edited an edit of one, as the core gets
// it; nil for the bot's own, another bot's, and those that aren't a
// person's message or reply.
func (c *Client) received(m *message, edited int64) bot.Received {
	self := c.selfID()
	if m.Author == nil || m.Author.Bot || m.WebhookID != "" || m.Type != 0 && m.Type != 19 || self == "" || m.Author.ID == self {
		return nil
	}
	c.seen(m)
	msg := c.message(m, edited)
	switch {
	case msg.Text == "" && len(msg.Media) == 0:
		return nil
	case msg.Addressed:
		return bot.Incoming{Chat: msg.Chat, Load: func(ctx context.Context, learn func(name, id string)) (feature.Message, bool) {
			if msg.Chat.GroupID != "" {
				if msg.Name != "" {
					learn(msg.Name, msg.Sender)
				}
				for _, u := range m.Mentions {
					if n := nameOf(&u, u.Member); n != "" && u.ID != self {
						learn(n, prefix+u.ID)
					}
				}
			}
			return msg, true
		}}
	case msg.Chat.GroupID != "":
		return bot.Chatter{Message: msg}
	}
	return nil
}

// chatOf is the chat of a message in channel: a guild's channel, or a
// thread, which is a channel of its own, or a DM with sender.
func chatOf(guild, channel, sender string) feature.Chat {
	if guild == "" {
		return feature.Chat{User: prefix + sender}
	}
	return feature.Chat{GroupID: prefix + channel}
}

// message is m as the core and features see it, with edited when it is an
// edit: the bot's own mention taken out, others' written @Name, and its
// attachments and the quoted message's.
func (c *Client) message(m *message, edited int64) feature.Message {
	self := c.selfID()
	body, mentioned := c.text(m, m.GuildID)
	msg := feature.Message{Chat: chatOf(m.GuildID, m.ChannelID, m.Author.ID), Sender: prefix + m.Author.ID, Name: nameOf(m.Author, m.Member),
		Text: body, Timestamp: snowflake(m.ID), Edited: edited}
	var images, files []feature.Media
	// add adds m's media, and reports whether text, m's, is only the link
	// of a GIF or picture it added.
	add := func(m *message, quoted bool, text string) (linkOnly bool) {
		for _, a := range m.Attachments {
			switch {
			case img.Supported(a.ContentType) && a.Size <= img.MaxReceived && len(images) < feature.MaxPictures:
				images = append(images, feature.Media{ID: a.URL, Quoted: quoted})
			case len(files) < feature.MaxFiles:
				u := a.URL
				if a.Size > img.MaxReceived {
					u = "" // too big to fetch: noted, not fetched
				}
				files = append(files, feature.Media{ID: u, Quoted: quoted, File: true, Name: a.Filename, MIME: a.ContentType, Size: a.Size})
			}
		}
		for _, e := range m.Embeds {
			if u := e.picture(); u != "" && len(images) < feature.MaxPictures {
				images = append(images, feature.Media{ID: u, Quoted: quoted})
				linkOnly = linkOnly || e.URL != "" && text == e.URL
			}
		}
		return linkOnly
	}
	if add(m, false, body) {
		msg.Text = "" // the model sees the GIF, not its link
	}
	var stickers []feature.Media
	for _, st := range m.Stickers {
		ext := map[int]string{1: ".png", 2: ".png", 4: ".gif"}[st.Format] // 3 is Lottie, which can't be shown
		if ext == "" || snowflake(st.ID) <= 0 || len(stickers) > 0 {
			continue
		}
		label := ""
		if n := txt.SafeName(st.Name); n != "" {
			label = " named " + strconv.Quote(txt.Clamp(n, 40))
		}
		stickers = append(stickers, feature.Media{ID: c.stickers + "/" + st.ID + ext, Sticker: true, Label: label})
	}
	replyToBot := false
	if r := m.Referenced; m.Reference != nil && r != nil && r.Author != nil {
		replyToBot = r.Author.ID == self || c.authorOf(snowflake(r.ID)) == prefix+self
		text, _ := c.text(r, m.GuildID)
		msg.Quote = &feature.Quote{Text: text, Author: nameOf(r.Author, r.Member), Bot: replyToBot, Sender: prefix + r.Author.ID, Sent: snowflake(r.ID)}
		if add(r, true, text) {
			msg.Quote.Text = ""
		}
	} else if m.Reference != nil {
		replyToBot = c.authorOf(snowflake(m.Reference.MessageID)) == prefix+self
	}
	msg.Addressed = m.GuildID == "" || mentioned || replyToBot || c.isCommand(body)
	msg.Media = slices.Concat(images, files, stickers)
	return msg
}

// isCommand reports whether text starts with one of the bot's commands,
// typed rather than chosen from Discord's list, which then counts as
// addressed in a channel, as on Signal; another slash word, such as
// /shrug, doesn't.
func (c *Client) isCommand(text string) bool {
	name, _, ok := feature.ParseCommand(text)
	if p := c.commands.Load(); ok && p != nil {
		return slices.ContainsFunc(*p, func(cmd bot.Command) bool { return strings.EqualFold(cmd.Name, name) })
	}
	return false
}

// mention is a user's mention in a message's text, <@id> or <@!id>.
var mention = regexp.MustCompile(`<@!?([0-9]+)>`)

// roleMention is a role's mention in a message's text, <@&id>.
var roleMention = regexp.MustCompile(`<@&([0-9]+)>`)

// text is m's text, posted in guild, with the bot's mention taken out,
// whether of the bot itself or of its own role there (which Discord's
// autocomplete often picks), and others' written @Name; mentioned says
// the bot's was there.
func (c *Client) text(m *message, guild string) (string, bool) {
	self, mentioned := c.selfID(), false
	c.mu.Lock()
	role := c.botRoles[guild]
	c.mu.Unlock()
	s := m.Content
	if role != "" {
		mentioned = slices.Contains(m.Roles, role)
		s = roleMention.ReplaceAllStringFunc(s, func(s string) string {
			if roleMention.FindStringSubmatch(s)[1] == role {
				mentioned = true
				return ""
			}
			return s
		})
	}
	s = mention.ReplaceAllStringFunc(s, func(s string) string {
		uid := mention.FindStringSubmatch(s)[1]
		if uid == self {
			mentioned = true
			return ""
		}
		for _, u := range m.Mentions {
			if u.ID == uid {
				return "@" + cmp.Or(nameOf(&u, u.Member), "[unnamed user]")
			}
		}
		return "@[unnamed user]"
	})
	return strings.TrimSpace(strings.Join(strings.FieldsFunc(s, func(r rune) bool { return r == ' ' }), " ")), mentioned
}

// maxGroups caps the channels the client remembers.
const maxGroups = 2000

// seen notes who posted m and where, for Groups, Delivered's members,
// Redact, a DM's channel, and a later delete's sender.
func (c *Client) seen(m *message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	uid, name := m.Author.ID, nameOf(m.Author, m.Member)
	c.noteAuthor(snowflake(m.ID), prefix+uid)
	if m.GuildID == "" {
		if _, ok := c.dms[uid]; ok || len(c.dms) < 10000 {
			c.dms[uid], c.dmUsers[m.ChannelID] = m.ChannelID, uid
		}
		return
	}
	if _, ok := c.channels[m.ChannelID]; !ok {
		c.addChannel(m.ChannelID, channel{guild: m.GuildID})
	}
	people := c.chats[m.ChannelID]
	if people == nil && len(c.chats) < maxGroups {
		people = map[string]string{}
		c.chats[m.ChannelID] = people
	}
	if name == "" || people == nil || len(people) >= 1000 || len(c.people) >= 10000 {
		return
	}
	people[uid], c.people[uid] = name, name
	in := c.seenIn[uid]
	if in == nil {
		in = map[string]bool{}
		c.seenIn[uid] = in
	}
	if len(in) < 200 {
		in[m.ChannelID] = true
	}
}

// addChannel notes a channel the bot can post in, under c.mu.
func (c *Client) addChannel(id string, ch channel) {
	if old, ok := c.channels[id]; ok || len(c.channels) < maxGroups {
		ch.name, ch.guildName = cmp.Or(ch.name, old.name), cmp.Or(ch.guildName, old.guildName)
		c.channels[id] = ch
		delete(c.left, id)
	}
}

// leave notes that the bot can no longer post in a channel, under c.mu.
func (c *Client) leave(id string) {
	delete(c.channels, id)
	delete(c.chats, id)
	if len(c.left) < 2*maxGroups {
		c.left[id] = true
	}
}

// noteAuthor remembers who sent a message, at most the last 5000, under
// c.mu: Discord's deletes don't say.
func (c *Client) noteAuthor(msgID int64, sender string) {
	if _, ok := c.authors[msgID]; ok || msgID == 0 {
		return
	}
	c.authors[msgID] = sender
	if c.authorQ = append(c.authorQ, msgID); len(c.authorQ) > 5000 {
		delete(c.authors, c.authorQ[0])
		c.authorQ = c.authorQ[1:]
	}
}

// authorOf is who sent a recent message, or "".
func (c *Client) authorOf(msgID int64) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.authors[msgID]
}

// deleted is a message deleted, as the core gets it: one the client saw,
// whose sender it knows.
func (c *Client) deleted(d json.RawMessage) []bot.Received {
	var e struct {
		ID        string `json:"id"`
		ChannelID string `json:"channel_id"`
		GuildID   string `json:"guild_id"`
	}
	if json.Unmarshal(d, &e) != nil {
		return nil
	}
	c.mu.Lock()
	sender, dmUser := c.authors[snowflake(e.ID)], c.dmUsers[e.ChannelID]
	c.mu.Unlock()
	if sender == "" || e.GuildID == "" && dmUser == "" {
		return nil
	}
	r := bot.Deleted{Chat: chatOf(e.GuildID, e.ChannelID, dmUser), Sender: sender, Sent: snowflake(e.ID), At: math.MaxInt64} // every version goes
	if sender == prefix+c.selfID() {
		r.Sender, r.Bot = "", true
	}
	return []bot.Received{r}
}

// reacted is a reaction put on a message or taken off, as the core gets
// it, but the bot's own. A server's own emoji, which has an ID, is
// :name:, its name made safe.
func (c *Client) reacted(d json.RawMessage, removed bool) []bot.Received {
	var e struct {
		UserID    string  `json:"user_id"`
		ChannelID string  `json:"channel_id"`
		MessageID string  `json:"message_id"`
		GuildID   string  `json:"guild_id"`
		Author    string  `json:"message_author_id"`
		Member    *member `json:"member"`
		Emoji     struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"emoji"`
	}
	if json.Unmarshal(d, &e) != nil || e.UserID == "" || e.UserID == c.selfID() || e.Emoji.Name == "" {
		return nil
	}
	emoji := txt.Clamp(e.Emoji.Name, 16)
	if e.Emoji.ID != "" {
		if emoji = txt.SafeName(e.Emoji.Name); emoji == "" {
			return nil
		}
		emoji = ":" + txt.Clamp(emoji, 32) + ":"
	}
	sent := snowflake(e.MessageID)
	author := c.authorOf(sent)
	if author == "" && e.Author != "" {
		author = prefix + e.Author
	}
	byBot := author == prefix+c.selfID()
	by := feature.Person{ID: prefix + e.UserID}
	if e.Member != nil {
		by.Name = nameOf(e.Member.User, e.Member)
	}
	if by.Name == "" {
		c.mu.Lock()
		by.Name = c.people[e.UserID]
		c.mu.Unlock()
	}
	return []bot.Received{bot.Happened{Event: feature.Event{Chat: chatOf(e.GuildID, e.ChannelID, e.UserID), By: by, Reaction: &feature.Reaction{
		Emoji: emoji, Removed: removed, Author: author, Bot: byBot, Sent: sent}}}}
}

// guild is a guild the bot is in, as the gateway sends at the start and
// when the bot joins one, or one it left: each of its channels changed.
// A guild that is only unavailable for a while is left as it was.
func (c *Client) guild(d json.RawMessage, gone bool) []bot.Received {
	var g struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Unavailable bool   `json:"unavailable"`
		Channels    []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"channels"`
		Threads []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"threads"`
		Roles []role `json:"roles"`
	}
	if json.Unmarshal(d, &g) != nil || g.Unavailable {
		return nil
	}
	var out []bot.Received
	c.mu.Lock()
	defer c.mu.Unlock()
	if gone {
		delete(c.botRoles, g.ID)
		for id, ch := range c.channels {
			if ch.guild == g.ID {
				c.leave(id)
				out = append(out, bot.GroupChanged{Chat: feature.Chat{GroupID: prefix + id}})
			}
		}
		return out
	}
	for _, r := range g.Roles {
		c.noteRole(g.ID, r)
	}
	for _, ch := range slices.Concat(g.Channels, g.Threads) {
		c.addChannel(ch.ID, channel{guild: g.ID, name: txt.SafeName(ch.Name), guildName: txt.SafeName(g.Name)})
		out = append(out, bot.GroupChanged{Chat: feature.Chat{GroupID: prefix + ch.ID}})
	}
	return out
}

// role is a guild's role; one Discord made for a bot names it in its tags.
type role struct {
	ID   string `json:"id"`
	Tags *struct {
		BotID string `json:"bot_id"`
	} `json:"tags"`
}

// noteRole remembers r if it is the bot's own role in guild, under c.mu.
func (c *Client) noteRole(guild string, r role) {
	if r.Tags == nil || r.Tags.BotID == "" || r.Tags.BotID != c.selfID() || r.ID == "" {
		return
	}
	if _, ok := c.botRoles[guild]; ok || len(c.botRoles) < maxGroups {
		c.botRoles[guild] = r.ID
	}
}

// roleChanged notes a guild's role made, changed or deleted: the bot's
// own, which a mention of addresses it.
func (c *Client) roleChanged(d json.RawMessage, deleted bool) {
	var e struct {
		GuildID string `json:"guild_id"`
		Role    role   `json:"role"`
		RoleID  string `json:"role_id"`
	}
	if json.Unmarshal(d, &e) != nil || e.GuildID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if deleted {
		if c.botRoles[e.GuildID] == e.RoleID {
			delete(c.botRoles, e.GuildID)
		}
		return
	}
	c.noteRole(e.GuildID, e.Role)
}

// Delivered readies a message a feature delivers as its sender, in its
// slot: its sender's name, the bot's @name taken out, and in a channel its
// members, the people seen posting there.
func (c *Client) Delivered(ctx context.Context, msg feature.Message) (bot.Ready, bool) {
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if c.WaitID(wait) == "" {
		logx.Warnf("delivered message dropped: not connected to Discord after 10s")
		return bot.Ready{}, false
	}
	var r bot.Ready
	c.mu.Lock()
	msg.Name = cmp.Or(msg.Name, c.people[strings.TrimPrefix(msg.Sender, prefix)])
	if g, ok := strings.CutPrefix(msg.Chat.GroupID, prefix); ok {
		for uid, n := range c.chats[g] {
			r.Members = append(r.Members, bot.Member{Name: n, ID: prefix + uid})
		}
	}
	c.mu.Unlock()
	slices.SortFunc(r.Members, func(a, b bot.Member) int { return strings.Compare(a.ID, b.ID) })
	rs := []rune(msg.Text)
	if sp := mdstyle.FindAt(rs, map[string]string{c.usernameOf(): "", c.BotName(): ""}); len(sp) > 0 {
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
// DM, or a channel it hasn't been removed from. (A user who blocked the
// bot refuses the send itself.)
func (c *Client) Reachable(ctx context.Context, chat feature.Chat) bool {
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if c.WaitID(wait) == "" {
		logx.Warnf("not connected to Discord after 10s")
		return false
	}
	if chat.GroupID == "" {
		return true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.left[strings.TrimPrefix(chat.GroupID, prefix)]
}

// Groups lists the channels where the bot has seen user post since it
// started, by name.
func (c *Client) Groups(ctx context.Context, user string) ([]feature.Group, error) {
	c.mu.Lock()
	var out []feature.Group
	for ch := range c.seenIn[strings.TrimPrefix(user, prefix)] {
		if info, ok := c.channels[ch]; ok {
			name := "#" + cmp.Or(info.name, ch)
			if info.guildName != "" {
				name += " (" + info.guildName + ")"
			}
			out = append(out, feature.Group{ID: prefix + ch, Name: name})
		}
	}
	c.mu.Unlock()
	slices.SortFunc(out, func(a, b feature.Group) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// discordID is a Discord ID: a number of 17 digits or more.
var discordID = regexp.MustCompile(`\b\d{17,20}\b`)

// Redact replaces each name the bot has seen with "redacted", as a whole
// word ignoring capitals, longest first, and every number long enough to
// be a Discord ID. It is for text the bot logs but doesn't control.
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
	return discordID.ReplaceAllString(s, "redacted")
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

package signal

import (
	"cmp"
	"context"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/img"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// envelope is a message signal-cli received.
type envelope struct {
	Source       string       `json:"source"`
	SourceNumber string       `json:"sourceNumber"`
	SourceUUID   string       `json:"sourceUuid"`
	SourceName   string       `json:"sourceName"`
	Timestamp    int64        `json:"timestamp"`
	DataMessage  *dataMessage `json:"dataMessage"`
	EditMessage  *struct {
		TargetSentTimestamp int64        `json:"targetSentTimestamp"` // the original's timestamp
		DataMessage         *dataMessage `json:"dataMessage"`         // the message as it now reads
	} `json:"editMessage"`
	SyncMessage *struct {
		SentMessage *syncSent `json:"sentMessage"`
	} `json:"syncMessage"` // what the account did on a linked device
	edited int64 // made from an EditMessage: when it was edited; DataMessage is the edit, Timestamp the original's
}

// syncSent is a message the account sent from a linked device: the parts
// the bot reads, a delete or an edit of one of its messages.
type syncSent struct {
	Destination       string     `json:"destination"`
	DestinationNumber string     `json:"destinationNumber"`
	DestinationUUID   string     `json:"destinationUuid"`
	GroupInfo         *groupInfo `json:"groupInfo"`
	RemoteDelete      *struct {
		TargetTimestamp int64 `json:"targetTimestamp"`
	} `json:"remoteDelete"`
	EditMessage *struct {
		TargetSentTimestamp int64 `json:"targetSentTimestamp"`
	} `json:"editMessage"`
}

type dataMessage struct {
	Message          string       `json:"message"`
	Mentions         []mention    `json:"mentions"`
	Quote            *quote       `json:"quote"`
	GroupInfo        *groupInfo   `json:"groupInfo"`
	Attachments      []attachment `json:"attachments"`
	Sticker          *sticker     `json:"sticker"`
	Reaction         *reaction    `json:"reaction"`
	ExpiresInSeconds int          `json:"expiresInSeconds"` // the disappearing-messages timer, 0 if off
	RemoteDelete     *struct {
		TargetTimestamp int64 `json:"targetTimestamp"` // the deleted message's timestamp
	} `json:"remoteDelete"`
	AdminDelete *struct { // a group admin deleting someone's message for everyone
		TargetAuthor        string `json:"targetAuthor"`
		TargetAuthorNumber  string `json:"targetAuthorNumber"`
		TargetAuthorUUID    string `json:"targetAuthorUuid"`
		TargetSentTimestamp int64  `json:"targetSentTimestamp"` // the deleted message's timestamp
	} `json:"adminDelete"`
}

// allMentions is the message's mentions and its quote's.
func (d *dataMessage) allMentions() []mention {
	if d.Quote == nil {
		return d.Mentions
	}
	return slices.Concat(d.Mentions, d.Quote.Mentions)
}

// reaction is an emoji put on a message, or taken back.
type reaction struct {
	Emoji               string `json:"emoji"`
	TargetAuthor        string `json:"targetAuthor"`
	TargetAuthorNumber  string `json:"targetAuthorNumber"`
	TargetAuthorUUID    string `json:"targetAuthorUuid"`
	TargetSentTimestamp int64  `json:"targetSentTimestamp"`
	IsRemove            bool   `json:"isRemove"`
}

// mention is an @mention; Start and Length count UTF-16 code units.
type mention struct {
	Name   string `json:"name"`
	Number string `json:"number"`
	UUID   string `json:"uuid"`
	Start  int    `json:"start"`
	Length int    `json:"length"`
}

// quote is the message one replies to. Its attachments carry only a small
// thumbnail of the original.
type quote struct {
	ID          int64        `json:"id"` // the quoted message's timestamp
	Author      string       `json:"author"`
	AuthorUUID  string       `json:"authorUuid"`
	Text        string       `json:"text"`
	Mentions    []mention    `json:"mentions"` // in Text, each a U+FFFC, as in a message
	Attachments []attachment `json:"attachments"`
}

type attachment struct {
	ContentType string      `json:"contentType"`
	Filename    string      `json:"filename"`
	ID          string      `json:"id"`
	Size        int         `json:"size"`
	Thumbnail   *attachment `json:"thumbnail"`
}

type sticker struct {
	PackID    string `json:"packId"`
	StickerID int    `json:"stickerId"`
}

// stickerOf parses a sticker's feature.Media ID, "<packId>:<stickerId>".
func stickerOf(id string) sticker {
	pack, n, _ := strings.Cut(id, ":")
	i, _ := strconv.Atoi(n)
	return sticker{pack, i}
}

type groupInfo struct {
	GroupID string `json:"groupId"`
	Type    string `json:"type"` // "UPDATE" when its members, name or description changed
}

// image is the attachment if it is an image the model can be shown, else
// its thumbnail if that is, else nil.
func (a *attachment) image() *attachment {
	for _, c := range []*attachment{a, a.Thumbnail} {
		if c != nil && c.ID != "" && c.Size <= img.MaxReceived && img.Supported(c.ContentType) {
			return c
		}
	}
	return nil
}

// file is the attachment as a file that isn't an image, if it can be
// fetched and isn't too big, else nil.
func (a *attachment) file() *attachment {
	if a.ID != "" && a.Size <= img.MaxReceived && a.image() == nil {
		return a
	}
	return nil
}

// is reports whether id is one of ids, and not "".
func is(id string, ids ...string) bool { return id != "" && slices.Contains(ids, id) }

// sender is the sender's ID: an ACI, else a number.
func (e *envelope) sender() string { return cmp.Or(e.SourceUUID, e.Source, e.SourceNumber) }

// chat is the chat e arrived in: its group, else a DM with its sender.
func (e *envelope) chat() feature.Chat {
	if d := e.DataMessage; d != nil && d.GroupInfo != nil && d.GroupInfo.GroupID != "" {
		return feature.Chat{GroupID: d.GroupInfo.GroupID}
	}
	return feature.Chat{User: e.sender()}
}

func fromSelf(env *envelope, self string) bool {
	return is(self, env.SourceUUID, env.Source, env.SourceNumber)
}

func isAllowed(env *envelope, allowed map[string]bool) bool {
	return allowed[env.SourceUUID] || allowed[env.Source] || allowed[env.SourceNumber]
}

// isAddressed reports whether env is for the bot: it mentions the bot,
// replies to one of its messages, or is a DM from an allowed sender, and
// has text, an attachment the bot can fetch or a sticker. It never blocks,
// so the reader can use it.
func isAddressed(env *envelope, self string, allowed map[string]bool) bool {
	d := env.DataMessage
	if d == nil || self == "" || fromSelf(env, self) {
		return false
	}
	if strings.TrimSpace(d.Message) == "" && d.Sticker == nil &&
		!slices.ContainsFunc(d.Attachments, func(a attachment) bool { return a.image() != nil || a.file() != nil }) {
		return false
	}
	if env.chat().GroupID == "" {
		return isAllowed(env, allowed)
	}
	return slices.ContainsFunc(d.Mentions, func(m mention) bool { return is(self, m.UUID, m.Number) }) ||
		d.Quote != nil && is(self, d.Quote.AuthorUUID, d.Quote.Author)
}

// event is env as the core gets it, decided on the reader goroutine without
// calling the network; false if the core has nothing to do with it.
func (c *Client) event(env *envelope) (bot.Received, bool) {
	// An edit is the message as it now reads, named by the original's
	// timestamp, which is how Signal names it (for a reaction or a quote).
	if e := env.EditMessage; env.DataMessage == nil && e != nil && e.DataMessage != nil && e.TargetSentTimestamp != 0 {
		edited := *env
		edited.DataMessage, edited.Timestamp, edited.EditMessage, edited.edited = e.DataMessage, e.TargetSentTimestamp, nil, cmp.Or(env.Timestamp, e.TargetSentTimestamp+1)
		env = &edited
	}
	d := env.DataMessage
	if d == nil {
		return c.synced(env)
	}
	chat := env.chat()
	if g := d.GroupInfo; g != nil && g.Type == "UPDATE" {
		c.refreshSoon() // so new members are known and departed ones forgotten
		self := c.self()
		c.members.changed(chat.GroupID, c.person(env.SourceUUID, env.Source, env.SourceNumber, env.SourceName), self != "" && fromSelf(env, self))
		return bot.GroupChanged{Chat: chat}, true
	}
	// Deletes and edits are honoured as the official Signal app would:
	// from the author, or a group admin's delete, whoever the bot answers.
	// SIGNAL_DM_USER_IDS says only who the bot answers, as message requests
	// do in the app, so it never stops one. What the app refuses, signal-cli
	// has already dropped: everything from a blocked person or group, a
	// group message from a non-member, and an admin delete from a
	// non-admin. Signal's time window needs no check, as history lasts 24
	// hours, within it.
	if rd := d.RemoteDelete; rd != nil { // deleted for everyone: forgotten here and in history
		if rd.TargetTimestamp == 0 {
			return nil, false
		}
		c.recent.forget(chat.Key(), env.sender(), rd.TargetTimestamp)
		return bot.Deleted{Chat: chat, Sender: env.sender(), Sent: rd.TargetTimestamp, At: math.MaxInt64}, true // every version goes
	}
	if ad := d.AdminDelete; ad != nil {
		author := cmp.Or(ad.TargetAuthorUUID, ad.TargetAuthor, ad.TargetAuthorNumber)
		if chat.GroupID == "" || author == "" || ad.TargetSentTimestamp == 0 {
			return nil, false
		}
		c.recent.forget(chat.Key(), author, ad.TargetSentTimestamp)
		return bot.Deleted{Chat: chat, Sender: author, Sent: ad.TargetSentTimestamp, At: math.MaxInt64}, true
	}
	if env.edited != 0 && chat.GroupID == "" && !isAllowed(env, c.allowed) {
		// An edit from someone the bot doesn't answer, such as one removed
		// from SIGNAL_DM_USER_IDS since: not answered or kept, but the
		// original goes. From someone never allowed there is nothing to go.
		return bot.Deleted{Chat: chat, Sender: env.sender(), Sent: env.Timestamp, At: env.edited}, true
	}
	if r := d.Reaction; r != nil {
		return c.reacted(env, chat, r)
	}
	if env.edited == 0 && (chat.GroupID != "" || isAllowed(env, c.allowed)) { // so a reply can fetch them; an edit keeps the original's
		c.recent.add(chat.Key(), env.sender(), env.Timestamp, d.Attachments)
	}
	load := func(ctx context.Context, learn func(name, id string)) (feature.Message, bool) {
		return c.load(ctx, env, learn)
	}
	self := c.self()
	if self == "" {
		return bot.Incoming{Chat: chat, Load: load}, true
	}
	switch addressed, other := c.judge(env, self); {
	case addressed:
		return bot.Incoming{Chat: chat, Load: load}, true
	case other:
		return bot.Chatter{Message: c.message(env, self)}, true
	}
	return nil, false
}

// synced is a delete or edit the account made on a linked device, as the
// core gets it: the bot's message it names leaves history, as the app on
// that device would drop it. An edit drops it too, so history never holds
// what the chat no longer shows. Anything else synced is nothing to the bot.
func (c *Client) synced(env *envelope) (bot.Received, bool) {
	s := env.SyncMessage
	if s == nil || s.SentMessage == nil {
		return nil, false
	}
	if self := c.self(); self != "" && !fromSelf(env, self) { // a sync is the account's own
		return nil, false
	}
	m := s.SentMessage
	var target int64
	switch {
	case m.RemoteDelete != nil:
		target = m.RemoteDelete.TargetTimestamp
	case m.EditMessage != nil:
		target = m.EditMessage.TargetSentTimestamp
	}
	chat := feature.Chat{User: cmp.Or(m.DestinationUUID, m.Destination, m.DestinationNumber)}
	if g := m.GroupInfo; g != nil && g.GroupID != "" {
		chat = feature.Chat{GroupID: g.GroupID}
	}
	if target == 0 || chat == (feature.Chat{}) {
		return nil, false
	}
	return bot.Deleted{Chat: chat, Bot: true, Sent: target, At: math.MaxInt64}, true
}

// reacted is a reaction as the core gets it: never the bot's own, nor one
// in a DM from someone not allowed, nor before the bot's ID is known.
func (c *Client) reacted(env *envelope, chat feature.Chat, r *reaction) (bot.Received, bool) {
	self := c.self()
	if self == "" || fromSelf(env, self) || chat.GroupID == "" && !isAllowed(env, c.allowed) || r.Emoji == "" {
		return nil, false
	}
	e := feature.Event{Chat: chat, By: c.person(env.SourceUUID, env.Source, env.SourceNumber, env.SourceName), Reaction: &feature.Reaction{
		Emoji: txt.Clamp(r.Emoji, 16), Removed: r.IsRemove, Author: cmp.Or(r.TargetAuthorUUID, r.TargetAuthor, r.TargetAuthorNumber),
		Bot: is(self, r.TargetAuthorUUID, r.TargetAuthor, r.TargetAuthorNumber), Sent: r.TargetSentTimestamp,
	}}
	return bot.Happened{Event: e}, true
}

// person is someone by their IDs, an ACI first, named as the bot knows
// them, else as their envelope did.
func (c *Client) person(uuid, source, number, sent string) feature.Person {
	return feature.Person{ID: cmp.Or(uuid, source, number), Name: txt.SafeName(cmp.Or(c.name(uuid, source, number), sent))}
}

// judge decides whether env is addressed, or another group message, for
// the hooks; it is neither when it is the bot's own, or a DM from someone
// not allowed, which is logged by ID only.
func (c *Client) judge(env *envelope, self string) (addressed, other bool) {
	switch {
	case isAddressed(env, self, c.allowed):
		return true, false
	case fromSelf(env, self):
	case env.chat().GroupID == "":
		if !isAllowed(env, c.allowed) {
			logx.Infof("%s ignored because not in SIGNAL_DM_USER_IDS (add this ID to allow DMs)", cmp.Or(env.sender(), "unknown sender"))
		}
	default:
		return false, true
	}
	return false, false
}

// load completes env in its slot, waiting up to 10s for the bot's ID; for
// an addressed message it loads the names and sticker labels it needs, and
// passes learn each name it shows the model.
func (c *Client) load(ctx context.Context, env *envelope, learn func(name, id string)) (feature.Message, bool) {
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	self := c.WaitID(wait)
	cancel()
	if self == "" {
		return feature.Message{}, false
	}
	if addressed, other := c.judge(env, self); !addressed {
		return c.message(env, self), other
	}
	c.loadNames(ctx, env)
	msg := c.message(env, self)
	c.people(env, self, learn)
	for i := range msg.Media {
		if m := &msg.Media[i]; m.Sticker {
			m.Label = c.stickerLabel(ctx, stickerOf(m.ID))
		}
	}
	return msg, true
}

// message is env as the core and features see it.
func (c *Client) message(env *envelope, self string) feature.Message {
	d := cmp.Or(env.DataMessage, &dataMessage{})
	m := feature.Message{
		Chat:      env.chat(),
		Sender:    env.sender(),
		Name:      c.senderName(env),
		Text:      c.text(d.Message, d.Mentions, self),
		Expires:   time.Duration(d.ExpiresInSeconds) * time.Second,
		Timestamp: env.Timestamp,
		Edited:    env.edited,
		Addressed: isAddressed(env, self, c.allowed),
	}
	var images, files []feature.Media
	add := func(as []attachment, quoted bool) {
		for i := range as {
			if a := as[i].image(); a != nil && len(images) < feature.MaxPictures {
				images = append(images, feature.Media{ID: a.ID, Quoted: quoted})
			} else if a == nil && len(files) < feature.MaxFiles && (as[i].file() != nil || quoted && !img.Supported(as[i].ContentType)) {
				f := as[i].file()
				if f == nil { // a quoted file not remembered: noted, not fetched
					f = &attachment{ContentType: as[i].ContentType, Filename: as[i].Filename}
				}
				files = append(files, feature.Media{ID: f.ID, Quoted: quoted, File: true, Name: f.Filename, MIME: f.ContentType, Size: f.Size})
			}
		}
	}
	add(d.Attachments, false)
	if q := d.Quote; q != nil {
		// The quote carries only thumbnails; the original attachments, if
		// the message was received lately.
		quoted := q.Attachments
		for _, author := range []string{q.AuthorUUID, q.Author} {
			if as, ok := c.recent.get(m.Chat.Key(), author, q.ID); ok && author != "" {
				quoted = as
				break
			}
		}
		add(quoted, true)
		m.Quote = &feature.Quote{Text: c.text(q.Text, q.Mentions, self), Author: c.name(q.AuthorUUID, q.Author), Bot: is(self, q.AuthorUUID, q.Author),
			Sender: cmp.Or(q.AuthorUUID, q.Author), Sent: q.ID}
	}
	m.Media = slices.Concat(images, files)
	if s := d.Sticker; s != nil {
		m.Media = append(m.Media, feature.Media{ID: s.PackID + ":" + strconv.Itoa(s.StickerID), Sticker: true})
	}
	return m
}

// people passes learn the names a group message showed the model, its
// sender's and those it mentions, so "@Name" in a reply can become a real
// mention. A DM shows none.
func (c *Client) people(env *envelope, self string, learn func(name, id string)) {
	if env.chat().GroupID == "" {
		return
	}
	add := func(name, id string) {
		if name = txt.SafeName(name); name != "" && id != "" && id != self {
			learn(name, id)
		}
	}
	add(c.senderName(env), env.SourceUUID)
	for _, m := range env.DataMessage.allMentions() {
		if m.Number != self {
			add(c.resolve(m), m.UUID)
		}
	}
}

// text removes the bot's own mention from msg and shows other mentions as
// @Name, or [unnamed user]. They are applied from the end, sorted as
// signal-cli doesn't promise an order, so earlier offsets stay valid; one
// that overlaps another or falls outside msg is skipped.
func (c *Client) text(msg string, ms []mention, self string) string {
	u := utf16.Encode([]rune(msg))
	sorted := slices.Clone(ms)
	slices.SortStableFunc(sorted, func(a, b mention) int { return cmp.Compare(b.Start, a.Start) })
	applied := len(u) // the lowest offset already rewritten
	for _, m := range sorted {
		// Written so that no pair of in-range ints can overflow.
		if m.Start < 0 || m.Length < 0 || m.Start > len(u) || m.Length > len(u)-m.Start || m.Start+m.Length > applied {
			continue
		}
		var repl []uint16
		if !is(self, m.UUID, m.Number) {
			name := "[unnamed user]"
			if n := c.resolve(m); n != "" {
				name = "@" + n
			}
			repl = utf16.Encode([]rune(name))
		}
		u = slices.Replace(u, m.Start, m.Start+m.Length, repl...)
		applied = m.Start
	}
	return strings.TrimSpace(string(utf16.Decode(u)))
}

// Delivered readies a message a feature delivers, in its slot, as if its
// sender had just posted it, waiting up to 10s for signal-cli and the bot's
// own name: a DM's sender must still be allowed, by ACI or number, and a
// group's still a member. A drop is logged without content.
func (c *Client) Delivered(ctx context.Context, msg feature.Message) (bot.Ready, bool) {
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if c.WaitID(wait) == "" {
		logx.Warnf("delivered message dropped: not connected to signal-cli after 10s")
		return bot.Ready{}, false
	}
	c.loadNames(wait, &envelope{})
	if msg.Chat.GroupID == "" && !c.allowed[msg.Sender] && !c.allowed[c.number(msg.Sender)] {
		logx.Infof("delivered message from %s dropped because not in SIGNAL_DM_USER_IDS (add this ID to allow DMs)", cmp.Or(msg.Sender, "unknown sender"))
		return bot.Ready{}, false
	}
	msg.Name = cmp.Or(msg.Name, c.name(msg.Sender))
	var r bot.Ready
	if msg.Chat.GroupID != "" {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		gs, err := c.groups(ctx, msg.Sender)
		cancel()
		if err != nil {
			logx.Warnf("delivered message dropped: listGroups failed: %v", err)
			return bot.Ready{}, false
		}
		i := slices.IndexFunc(gs, func(g group) bool { return g.ID == msg.Chat.GroupID })
		if i < 0 {
			logx.Infof("delivered message dropped: its sender is not in the group")
			return bot.Ready{}, false
		}
		for _, m := range gs[i].Members {
			r.Members = append(r.Members, bot.Member{Name: c.name(m.UUID, m.Number), ID: m.UUID}) // a member without an ACI is never linked
		}
	}
	rs := []rune(msg.Text)
	s, mentioned := strip(styled{rs, make([]uint8, len(rs))}, c.BotName(), defaultName)
	msg.Text, r.Mentioned = string(s.text), mentioned
	r.Message = msg
	return r, true
}

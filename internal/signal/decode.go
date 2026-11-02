package signal

import (
	"cmp"
	"context"
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
	DataMessage  *dataMessage `json:"dataMessage"`
}

type dataMessage struct {
	Message          string       `json:"message"`
	Mentions         []mention    `json:"mentions"`
	Quote            *quote       `json:"quote"`
	GroupInfo        *groupInfo   `json:"groupInfo"`
	Attachments      []attachment `json:"attachments"`
	Sticker          *sticker     `json:"sticker"`
	ExpiresInSeconds int          `json:"expiresInSeconds"` // the disappearing-messages timer, 0 if off
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
	Author      string       `json:"author"`
	AuthorUUID  string       `json:"authorUuid"`
	Text        string       `json:"text"`
	Attachments []attachment `json:"attachments"`
}

type attachment struct {
	ContentType string      `json:"contentType"`
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
// has text, an image or a sticker. It never blocks, so the reader can use it.
func isAddressed(env *envelope, self string, allowed map[string]bool) bool {
	d := env.DataMessage
	if d == nil || self == "" || fromSelf(env, self) {
		return false
	}
	if strings.TrimSpace(d.Message) == "" && d.Sticker == nil &&
		!slices.ContainsFunc(d.Attachments, func(a attachment) bool { return a.image() != nil }) {
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
	d := env.DataMessage
	if d == nil {
		return bot.Received{}, false
	}
	chat := env.chat()
	if g := d.GroupInfo; g != nil && g.Type == "UPDATE" {
		c.refreshSoon() // so new members are known and departed ones forgotten
		return bot.Received{Chat: chat, GroupChange: true}, true
	}
	load := func(ctx context.Context, learn func(name, id string)) (feature.Message, bool) {
		return c.load(ctx, env, learn)
	}
	self := c.self()
	if self == "" {
		return bot.Received{Chat: chat, Load: load}, true
	}
	switch addressed, other := c.judge(env, self); {
	case addressed:
		return bot.Received{Chat: chat, Addressed: true, Load: load}, true
	case other:
		return bot.Received{Message: c.message(env, self)}, true
	}
	return bot.Received{}, false
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
			logx.Infof("%s ignored because not in SIGNAL_DM_USER_IDS", cmp.Or(env.sender(), "unknown sender"))
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
		Name:      txt.SafeName(env.SourceName),
		Text:      c.text(d.Message, d.Mentions, self),
		Expires:   time.Duration(d.ExpiresInSeconds) * time.Second,
		Addressed: isAddressed(env, self, c.allowed),
	}
	images := func(as []attachment, quoted bool) {
		for i := range as {
			if a := as[i].image(); a != nil && len(m.Media) < feature.MaxPictures {
				m.Media = append(m.Media, feature.Media{ID: a.ID, Quoted: quoted})
			}
		}
	}
	images(d.Attachments, false)
	if q := d.Quote; q != nil {
		images(q.Attachments, true)
		m.Quote = &feature.Quote{Text: q.Text, Author: c.name(q.AuthorUUID, q.Author), Bot: is(self, q.AuthorUUID, q.Author)}
	}
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
	add(env.SourceName, env.SourceUUID)
	for _, m := range env.DataMessage.Mentions {
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
func (c *Client) Delivered(ctx context.Context, msg feature.Message) (bot.Received, bool) {
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if c.WaitID(wait) == "" {
		logx.Warnf("delivered message dropped: not connected to signal-cli after 10s")
		return bot.Received{}, false
	}
	c.loadNames(wait, &envelope{})
	if msg.Chat.GroupID == "" && !c.allowed[msg.Sender] && !c.allowed[c.number(msg.Sender)] {
		logx.Infof("delivered message from %s dropped because not in SIGNAL_DM_USER_IDS", cmp.Or(msg.Sender, "unknown sender"))
		return bot.Received{}, false
	}
	msg.Name = cmp.Or(msg.Name, c.name(msg.Sender))
	var r bot.Received
	if msg.Chat.GroupID != "" {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		gs, err := c.groups(ctx, msg.Sender)
		cancel()
		if err != nil {
			logx.Warnf("delivered message dropped: listGroups failed: %v", err)
			return bot.Received{}, false
		}
		i := slices.IndexFunc(gs, func(g group) bool { return g.ID == msg.Chat.GroupID })
		if i < 0 {
			logx.Infof("delivered message dropped: its sender is not in the group")
			return bot.Received{}, false
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

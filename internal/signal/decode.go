package signal

import (
	"cmp"
	"context"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/img"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// unnamed stands in for a mention whose name isn't known.
const unnamed = "[unnamed user]"

// envelope is a message signal-cli received.
type envelope struct {
	Source       string       `json:"source"`
	SourceNumber string       `json:"sourceNumber"`
	SourceUUID   string       `json:"sourceUuid"`
	SourceName   string       `json:"sourceName"`
	DataMessage  *dataMessage `json:"dataMessage"`
}

// dataMessage is what a received message carries.
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

// sticker names a sticker by its pack and its index in the pack.
type sticker struct {
	PackID    string `json:"packId"`
	StickerID int    `json:"stickerId"`
}

type groupInfo struct {
	GroupID string `json:"groupId"`
	Type    string `json:"type"` // "UPDATE" when its members, name or description changed
}

// image is the attachment if it is an image the model can be shown, else
// its thumbnail if that is, else nil.
func (a *attachment) image() *attachment {
	for _, c := range []*attachment{a, a.Thumbnail} {
		if c != nil && c.ID != "" && c.Size <= img.MaxBytes && img.Supported(c.ContentType) {
			return c
		}
	}
	return nil
}

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
	return env.SourceUUID == self || env.Source == self || env.SourceNumber == self
}

func isAllowed(env *envelope, allowed map[string]bool) bool {
	return allowed[env.SourceUUID] || allowed[env.Source] || allowed[env.SourceNumber]
}

// isAddressed reports whether env is for the bot: it mentions the bot,
// replies to one of its messages, or is a DM from an allowed sender, and
// has text, an image or a sticker. It never blocks or allocates, so the
// reader goroutine can decide with it.
func isAddressed(env *envelope, self string, allowed map[string]bool) bool {
	d := env.DataMessage
	if d == nil || self == "" || fromSelf(env, self) {
		return false
	}
	if strings.TrimSpace(d.Message) == "" && d.Sticker == nil &&
		!slices.ContainsFunc(d.Attachments, func(a attachment) bool { return a.image() != nil }) {
		return false
	}
	if d.GroupInfo == nil || d.GroupInfo.GroupID == "" {
		return isAllowed(env, allowed)
	}
	for _, m := range d.Mentions {
		if m.UUID == self || m.Number == self {
			return true
		}
	}
	q := d.Quote
	return q != nil && (q.AuthorUUID == self || q.Author == self)
}

// event is env as the core gets it, decided on the reader goroutine without
// calling the network; false if the core has nothing to do with it.
func (c *Client) event(env *envelope) (feature.Received, bool) {
	d := env.DataMessage
	if d == nil {
		return feature.Received{}, false
	}
	chat := env.chat()
	if g := d.GroupInfo; g != nil && g.Type == "UPDATE" {
		// Reload the contact list, so new members are known and departed
		// ones forgotten.
		c.refreshSoon()
		return feature.Received{Message: feature.Message{Chat: chat}, GroupChange: true}, true
	}
	load := func(ctx context.Context, learn func(name, id string)) (feature.Message, bool) {
		return c.load(ctx, env, learn)
	}
	self := c.self()
	if self == "" {
		return feature.Received{Message: feature.Message{Chat: chat}, Wait: true, Load: load}, true
	}
	switch addressed, other := c.judge(env, self); {
	case addressed:
		return feature.Received{Message: feature.Message{Chat: chat, Addressed: true}, Load: load}, true
	case other:
		return feature.Received{Message: c.message(env, self)}, true
	}
	return feature.Received{}, false
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

// load completes env in its slot: it waits up to 10s for the bot's ID if
// need be, and for an addressed message loads the names it needs and its
// sticker labels, and passes learn each name it shows the model.
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
		if m := &msg.Media[i]; m.Pack != "" {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			m.Label = c.stickerLabel(ctx, m.Pack, m.Sticker)
			cancel()
		}
	}
	return msg, true
}

// message is env as the core and features see it. self is the bot's ID.
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
				m.Media = append(m.Media, feature.Media{ID: a.ID, Type: a.ContentType, Quoted: quoted})
			}
		}
	}
	images(d.Attachments, false)
	if q := d.Quote; q != nil {
		images(q.Attachments, true)
		m.Quote = &feature.Quote{Text: q.Text, Author: c.name(q.AuthorUUID, q.Author), Bot: q.AuthorUUID == self || q.Author == self}
	}
	if s := d.Sticker; s != nil {
		m.Media = append(m.Media, feature.Media{Pack: s.PackID, Sticker: s.StickerID})
	}
	return m
}

// people passes learn the names a group message showed the model, so
// "@Name" in a reply can become a real mention: its sender's and those of
// the people it mentions. A DM shows none.
func (c *Client) people(env *envelope, self string, learn func(name, id string)) {
	d := env.DataMessage
	if d == nil || d.GroupInfo == nil || d.GroupInfo.GroupID == "" {
		return
	}
	add := func(name, id string) {
		if name = txt.SafeName(name); name != "" && id != "" && id != self {
			learn(name, id)
		}
	}
	add(env.SourceName, env.SourceUUID)
	for _, m := range d.Mentions {
		if m.Number != self {
			add(c.resolve(m), m.UUID)
		}
	}
}

// text removes the bot's own mention from msg and shows other mentions as
// @Name, or [unnamed user]. Mentions are applied from the end, so earlier
// offsets stay valid; signal-cli's order isn't guaranteed, so they are
// sorted, and one that overlaps another or falls outside msg is skipped.
func (c *Client) text(msg string, ms []mention, self string) string {
	u := utf16.Encode([]rune(msg))
	sorted := slices.Clone(ms)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Start > sorted[j].Start })
	applied := len(u) // the lowest offset already rewritten
	for _, m := range sorted {
		// Written so that no pair of in-range ints can overflow.
		if m.Start < 0 || m.Length < 0 || m.Start > len(u) || m.Length > len(u)-m.Start || m.Start+m.Length > applied {
			continue
		}
		var repl []uint16
		if m.UUID != self && m.Number != self {
			name := "@" + c.resolve(m)
			if name == "@" {
				name = unnamed
			}
			repl = utf16.Encode([]rune(name))
		}
		u = append(u[:m.Start], append(repl, u[m.Start+m.Length:]...)...)
		applied = m.Start
	}
	return strings.TrimSpace(string(utf16.Decode(u)))
}

// Delivered readies a message a feature delivers, in its slot, as if its
// sender had just posted it. It waits up to 10s for the bot to be connected
// and loads its own name. A DM's sender must still be allowed, by ACI or
// number, and a group's still a member; claim is passed each member's name
// and ID. It returns msg with its sender's name filled in and without the
// bot's @name, and whether that was there; false, logged without content,
// if msg is dropped.
func (c *Client) Delivered(ctx context.Context, msg feature.Message, claim func(name, id string)) (out feature.Message, mentioned, ok bool) {
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if c.WaitID(wait) == "" {
		logx.Warnf("delivered message dropped: not connected to signal-cli after 10s")
		return msg, false, false
	}
	c.loadNames(wait, &envelope{})
	if msg.Chat.GroupID == "" && !c.allowed[msg.Sender] && !c.allowed[c.number(msg.Sender)] {
		logx.Infof("delivered message from %s dropped because not in SIGNAL_DM_USER_IDS", cmp.Or(msg.Sender, "unknown sender"))
		return msg, false, false
	}
	msg.Name = cmp.Or(msg.Name, c.name(msg.Sender))
	if msg.Chat.GroupID != "" {
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		gs, err := c.groups(ctx)
		cancel()
		if err != nil {
			logx.Warnf("delivered message dropped: listGroups failed: %v", err)
			return msg, false, false
		}
		i := slices.IndexFunc(gs, func(g group) bool { return g.ID == msg.Chat.GroupID && g.has(msg.Sender) })
		if i < 0 {
			logx.Infof("delivered message dropped: its sender is not in the group")
			return msg, false, false
		}
		for _, m := range gs[i].members {
			claim(c.name(m.UUID, m.Number), m.UUID) // a member without an ACI is never linked
		}
	}
	msg.Text, mentioned = stripAt(msg.Text, c.BotName(), defaultName)
	return msg, mentioned, true
}

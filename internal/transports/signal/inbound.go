package signal

import (
	"cmp"
	"context"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

func fromSelf(env *Envelope, self string) bool {
	return env.SourceUUID == self || env.Source == self || env.SourceNumber == self
}

func isAllowed(env *Envelope, allowed map[string]bool) bool {
	return allowed[env.SourceUUID] || allowed[env.Source] || allowed[env.SourceNumber]
}

// trigger is the cheap, allocation-free half of the trigger rules: it says why
// the bot should answer ("mention", "reply" or "dm"), or "" to stay silent. It
// runs inline on the reader goroutine so that messages the bot will ignore
// cost nothing: no goroutine, no lock, no RPC.
func trigger(env *Envelope, self string, allowed map[string]bool) string {
	dm := env.DataMessage
	if dm == nil || self == "" || fromSelf(env, self) {
		return ""
	}
	// A photo or sticker alone (no text) counts too: in a DM, or as a reply
	// to the bot.
	if strings.TrimSpace(dm.Message) == "" && dm.Sticker == nil &&
		!slices.ContainsFunc(dm.Attachments, func(a Attachment) bool { return a.Image() != nil }) {
		return ""
	}
	if dm.GroupInfo == nil || dm.GroupInfo.GroupID == "" {
		if isAllowed(env, allowed) {
			return "dm"
		}
		return ""
	}
	for _, m := range dm.Mentions {
		if m.UUID == self || m.Number == self {
			return "mention"
		}
	}
	if q := dm.Quote; q != nil && (q.AuthorUUID == self || q.Author == self) {
		return "reply"
	}
	return ""
}

// chatOf is the chat an envelope belongs to.
func chatOf(env *Envelope) (chat.ChatID, bool) {
	if g := env.DataMessage.GroupInfo; g != nil && g.GroupID != "" {
		return groupChat(g.GroupID), true
	}
	return userChat(cmp.Or(env.SourceUUID, env.Source)), false
}

// inbound runs on the reader goroutine. It must not block: anything that
// may call the network waits in Load, which the core runs in a slot.
func (a *Adapter) inbound(env *Envelope, handle func(chat.Incoming)) {
	if env.DataMessage == nil {
		return
	}
	// A group changed (members, name or description): reload the contact list
	// so new members are known and departed ones are forgotten.
	if g := env.DataMessage.GroupInfo; g != nil && g.Type == "UPDATE" {
		a.Forget(groupChat(g.GroupID))
		a.refreshSoon()
	}
	c, group := chatOf(env)
	// Cheap inline rejection. Before the account id is known (the first moment
	// after connecting) we cannot judge, so it goes to Load to decide.
	if self := a.c.SelfID(); self != "" && trigger(env, self, a.allowed) == "" {
		// Not for the bot, and never logged. A group message goes to the core
		// as context; a DM from anyone else is dropped untouched. The one
		// thing said is that a DM came from someone who may not use the bot,
		// by id only, never the name or text.
		switch {
		case fromSelf(env, self):
		case group:
			handle(chat.Incoming{Chat: c, Group: true, Load: func(context.Context) (chat.Message, bool) {
				return a.message(env, c, "", self), true
			}})
		case !isAllowed(env, a.allowed):
			logx.Infof("%s ignored because not in SIGNAL_DM_USER_IDS",
				cmp.Or(env.SourceUUID, env.Source, env.SourceNumber, "unknown sender"))
		}
		return
	}
	handle(chat.Incoming{Chat: c, Group: group, Addressed: true, Load: func(ctx context.Context) (chat.Message, bool) {
		return a.load(ctx, env, c)
	}})
}

// load builds an addressed message. It may wait for the account id and load
// the contact list, so it runs in the core's slot.
func (a *Adapter) load(ctx context.Context, env *Envelope, c chat.ChatID) (chat.Message, bool) {
	self := a.c.WaitSelf(10 * time.Second)
	kind := trigger(env, self, a.allowed)
	if kind == "" {
		return chat.Message{}, false
	}
	a.names.Learn(env)
	// Load the contact list if someone other than the bot is mentioned whose
	// name we lack. Only messages the bot will answer get this far.
	if slices.ContainsFunc(env.DataMessage.Mentions, func(m Mention) bool {
		return m.UUID != self && m.Number != self && a.names.needsRefresh(m)
	}) {
		a.refreshContacts(ctx)
	}
	m := a.message(env, c, kind, self)
	if q := env.DataMessage.Quote; q != nil && q.Text != "" {
		m.Quote = &chat.Quote{Text: q.Text, FromBot: q.AuthorUUID == self || q.Author == self,
			Author: a.names.lookup(q.AuthorUUID, q.Author).name}
	}
	for i, md := range m.Media {
		if md.Kind == chat.Sticker {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			m.Media[i].Label = a.c.StickerLabel(ctx, *env.DataMessage.Sticker)
			cancel()
		}
	}
	if m.Text == "" && len(m.Media) == 0 {
		return chat.Message{}, false
	}
	if m.Group {
		a.remember(c, env, self)
	}
	return m, true
}

// message is what can be built from the envelope and the names already known.
func (a *Adapter) message(env *Envelope, c chat.ChatID, why, self string) chat.Message {
	dm := env.DataMessage
	m := chat.Message{Chat: c, Group: dm.GroupInfo != nil && dm.GroupInfo.GroupID != "", Why: why,
		Sender:  chat.Person{ID: cmp.Or(env.SourceUUID, env.Source), Name: txt.Safe(env.SourceName)},
		Text:    stripMentions(dm.Message, dm.Mentions, self, a.names),
		Expires: time.Duration(dm.ExpiresInSeconds) * time.Second}
	// Images: the message's own, then the quoted message's (a thumbnail).
	attached := dm.Attachments
	if dm.Quote != nil && why != "" {
		attached = append(slices.Clone(attached), dm.Quote.Attachments...)
	}
	for _, at := range attached {
		if img := at.Image(); img != nil {
			m.Media = append(m.Media, chat.Media{ID: img.ID, Kind: chat.Image})
		}
	}
	if s := dm.Sticker; s != nil {
		m.Media = append(m.Media, chat.Media{ID: s.PackID + ":" + strconv.Itoa(s.StickerID), Kind: chat.Sticker})
	}
	return m
}

// stripMentions removes the bot's own mention and expands other mentions to
// @name. Offsets are UTF-16 code units. Mentions are applied from the end so
// earlier offsets stay valid, which requires sorting: signal-cli's order is
// not guaranteed, and an out-of-order pair would splice over live text.
func stripMentions(msg string, ms []Mention, self string, names *Names) string {
	u := utf16.Encode([]rune(msg))
	sorted := append([]Mention(nil), ms...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Start > sorted[j].Start })

	applied := len(u) // lowest offset already rewritten
	for _, m := range sorted {
		// Written so that no pair of in-range int values can overflow.
		if m.Start < 0 || m.Length < 0 || m.Start > len(u) || m.Length > len(u)-m.Start {
			continue
		}
		if m.Start+m.Length > applied { // overlaps one we already applied
			continue
		}
		var repl []uint16
		if m.UUID != self && m.Number != self {
			name := "@" + names.Resolve(m)
			if name == "@" {
				logx.Debugf("unresolved mention of %s", cmp.Or(m.UUID, m.Number))
				name = unnamedUser
			}
			repl = utf16.Encode([]rune(name))
		}
		u = append(u[:m.Start], append(repl, u[m.Start+m.Length:]...)...)
		applied = m.Start
	}
	return strings.TrimSpace(string(utf16.Decode(u)))
}

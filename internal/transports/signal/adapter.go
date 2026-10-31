package signal

import (
	"context"
	"encoding/base64"
	"fmt"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"

	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/kit/bound"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// MaxChars is Signal's limit for one message; replies are split at it.
const MaxChars = 2000

// maxChats bounds the chats whose names are remembered: chat ids come from
// the network.
const maxChats = 500

// Adapter is the Signal network for the core: envelopes in as chat
// messages, replies out with real mentions.
type Adapter struct {
	c       *Client
	names   *Names
	allowed map[string]bool // senders who may DM the bot
	ctx     context.Context

	mu    sync.Mutex
	known map[chat.ChatID]map[string]string // chat -> name -> uuid shown to the model

	refreshPending atomic.Bool // a contact reload is already scheduled
}

// NewAdapter is the Signal network on the signal-cli socket at addr, with
// DMs allowed from the senders in allowed.
func NewAdapter(addr string, allowed map[string]bool) *Adapter {
	return &Adapter{c: New(addr), names: NewNames(), allowed: allowed, ctx: context.Background(),
		known: map[chat.ChatID]map[string]string{}}
}

// Client is the signal-cli client underneath.
func (a *Adapter) Client() *Client { return a.c }

func (a *Adapter) Kind() string { return "signal" }

// Run reads messages until ctx ends, reconnecting as needed, and loads the
// contact list after each connection.
func (a *Adapter) Run(ctx context.Context, handle func(chat.Incoming)) {
	a.ctx = ctx
	a.c.OnConnect = a.refreshSoon
	a.c.Run(ctx, func(env *Envelope) { a.inbound(env, handle) })
}

// Inject handles one envelope as if read from the socket: for tests.
func (a *Adapter) Inject(env *Envelope, handle func(chat.Incoming)) { a.inbound(env, handle) }

func userChat(id string) chat.ChatID  { return chat.ChatID("signal:u:" + id) }
func groupChat(id string) chat.ChatID { return chat.ChatID("signal:g:" + id) }

// target is where a chat's messages go.
func target(c chat.ChatID) Target {
	rest := strings.TrimPrefix(string(c), "signal:")
	if g, ok := strings.CutPrefix(rest, "g:"); ok {
		return Target{GroupID: g}
	}
	return Target{User: strings.TrimPrefix(rest, "u:")}
}

// refreshSoon reloads the contact list as soon as the rate limit allows. Calls
// made while a reload is already scheduled fold into it, so a burst of group
// changes costs one reload and at most one timer.
func (a *Adapter) refreshSoon() {
	if a.ctx.Err() != nil || !a.refreshPending.CompareAndSwap(false, true) {
		return
	}
	time.AfterFunc(a.names.untilRefreshAllowed(), func() {
		a.refreshPending.Store(false)
		if a.ctx.Err() == nil {
			a.refreshContacts(a.ctx)
		}
	})
}

// refreshContacts loads the contact list unless that was done moments ago,
// and reports whether it tried.
func (a *Adapter) refreshContacts(ctx context.Context) bool {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return a.names.Refresh(ctx, a.c)
}

// Send splits the text at Signal's limit, turns "@Name" for names known in
// the chat into real mentions, and sends the pictures with the first part.
func (a *Adapter) Send(ctx context.Context, m chat.Outgoing) error {
	parts := txt.Split(m.Text, MaxChars)
	var attachments []string
	for i, p := range m.Pictures {
		attachments = append(attachments, dataURI(p.Data, fmt.Sprintf("image%d", i+1)))
	}
	if len(parts) == 0 {
		if len(attachments) == 0 {
			return nil
		}
		parts = []string{""} // the pictures alone
	}
	known, t := a.knownNames(m.Chat), target(m.Chat)
	for i, part := range parts {
		var att []string
		if i == 0 {
			att = attachments
		}
		if err := a.c.Send(t, part, buildMentions(part, known), att); err != nil {
			return err
		}
	}
	return nil
}

// dataURI is an image as signal-cli takes an attachment.
func dataURI(data []byte, name string) string {
	mime := http.DetectContentType(data)
	ext := strings.TrimPrefix(mime, "image/")
	return "data:" + mime + ";filename=" + name + "." + ext + ";base64," + base64.StdEncoding.EncodeToString(data)
}

func (a *Adapter) Typing(c chat.ChatID) { a.c.Typing(target(c)) }

// Fetch downloads an image or sticker from a message in chat c.
func (a *Adapter) Fetch(ctx context.Context, c chat.ChatID, m chat.Media) ([]byte, error) {
	if m.Kind == chat.Sticker {
		pack, id, _ := strings.Cut(m.ID, ":")
		n, err := strconv.Atoi(id)
		if err != nil {
			return nil, err
		}
		return a.c.GetSticker(ctx, Sticker{PackID: pack, StickerID: n})
	}
	return a.c.GetAttachment(ctx, target(c), m.ID)
}

// Name is the bot's own profile name, loading the contact list once if it
// isn't known yet.
func (a *Adapter) Name(ctx context.Context) string {
	name := func() string { return a.names.lookup(a.c.SelfID()).name }
	if name() == "" && a.refreshContacts(ctx) && name() == "" {
		logx.Warnf("own profile name not found; calling itself signalbot (INTRO can name it)")
	}
	return name()
}

// Describe is the prompt text that sets the scene: a Signal chat, plain
// text, Signal's length limit, and how unknown mentions are shown.
func (a *Adapter) Describe() string {
	return "You are a helpful assistant in a Signal chat. " +
		"Reply in plain text: Signal does not render markdown. " +
		"Aim for a single line. Two or three sentences at the very most, and only when asked for detail. " +
		"Never exceed 2000 characters, which is Signal's limit for one message. " +
		"A mention shown as " + unnamedUser + " is someone whose name you don't know; don't invent one."
}

// Forget drops the names remembered for a chat: its history was erased, or
// its members changed. People still in it are remembered again by their
// next message.
func (a *Adapter) Forget(c chat.ChatID) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.known, c)
}

// buildMentions returns signal-cli mention specs ("start:length:uuid") for the
// known @names in text. Offsets are UTF-16 code units. An ambiguous name
// (empty uuid) stays plain text.
func buildMentions(text string, known map[string]string) []string {
	rs := []rune(text)
	var out []string
	u, pos := 0, 0 // UTF-16 offset of rs[pos]
	for _, s := range txt.AtNames(rs, known) {
		u += utf16Len(rs[pos:s.Start])
		length := utf16Len(rs[s.Start:s.End])
		if s.ID != "" {
			out = append(out, fmt.Sprintf("%d:%d:%s", u, length, s.ID))
		}
		u += length
		pos = s.End
	}
	return out
}

func utf16Len(rs []rune) (n int) {
	for _, r := range rs {
		n += utf16.RuneLen(r)
	}
	return n
}

// remember records the names the model is shown in a group message (the
// sender's, and those of people mentioned), so "@Name" in a reply can become
// a real mention.
func (a *Adapter) remember(c chat.ChatID, env *Envelope, self string) {
	seen := map[string]string{}
	add := func(n, uuid string) {
		if n = txt.Safe(n); n != "" && uuid != "" && uuid != self {
			bindName(seen, n, uuid)
		}
	}
	add(env.SourceName, env.SourceUUID) // the name the model sees in the prompt
	for _, m := range env.DataMessage.Mentions {
		if m.Number != self {
			add(a.names.Resolve(m), m.UUID)
		}
	}
	if len(seen) == 0 {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	bound.MakeRoom(a.known, c, maxChats)
	k := a.known[c]
	if k == nil {
		k = map[string]string{}
		a.known[c] = k
	}
	for n, uuid := range seen {
		if _, ok := k[n]; ok || len(k) < 256 {
			bindName(k, n, uuid)
		}
	}
}

// bindName records that name n refers to uuid. Display names are self-chosen,
// so anyone can claim a member's name: a name claimed by two different people
// is ambiguous, marked "", and never linked.
func bindName(m map[string]string, n, uuid string) {
	n = strings.ToLower(n) // mentions match case-insensitively, so "alice" claims "Alice"
	if old, ok := m[n]; !ok {
		m[n] = uuid
	} else if old != uuid {
		m[n] = ""
	}
}

func (a *Adapter) knownNames(c chat.ChatID) map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return maps.Clone(a.known[c])
}

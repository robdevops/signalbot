package signal

import (
	"cmp"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/img"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

const (
	// MaxChars is Signal's limit for one message; replies are split at it.
	MaxChars = 2000
	// MaxReply caps a whole reply, so one request can't fan out into
	// hundreds of messages.
	MaxReply = 4 * MaxChars
	// unnamed stands in for a mention whose name isn't known.
	unnamed   = "[unnamed user]"
	truncated = " […truncated]"
)

// Envelope is a message signal-cli received.
type Envelope struct {
	Source       string       `json:"source"`
	SourceNumber string       `json:"sourceNumber"`
	SourceUUID   string       `json:"sourceUuid"`
	SourceName   string       `json:"sourceName"`
	DataMessage  *DataMessage `json:"dataMessage"`
}

// DataMessage is what a received message carries.
type DataMessage struct {
	Message          string       `json:"message"`
	Mentions         []Mention    `json:"mentions"`
	Quote            *Quote       `json:"quote"`
	GroupInfo        *GroupInfo   `json:"groupInfo"`
	Attachments      []Attachment `json:"attachments"`
	Sticker          *Sticker     `json:"sticker"`
	ExpiresInSeconds int          `json:"expiresInSeconds"` // the disappearing-messages timer, 0 if off
}

// Mention is an @mention; Start and Length count UTF-16 code units.
type Mention struct {
	Name   string `json:"name"`
	Number string `json:"number"`
	UUID   string `json:"uuid"`
	Start  int    `json:"start"`
	Length int    `json:"length"`
}

// Quote is the message one replies to. Its attachments carry only a small
// thumbnail of the original.
type Quote struct {
	Author      string       `json:"author"`
	AuthorUUID  string       `json:"authorUuid"`
	Text        string       `json:"text"`
	Attachments []Attachment `json:"attachments"`
}

// Attachment is a file on a message.
type Attachment struct {
	ContentType string      `json:"contentType"`
	ID          string      `json:"id"`
	Size        int         `json:"size"`
	Thumbnail   *Attachment `json:"thumbnail"`
}

// Sticker names a sticker by its pack and its index in the pack.
type Sticker struct {
	PackID    string `json:"packId"`
	StickerID int    `json:"stickerId"`
}

// GroupInfo says which group a message is in.
type GroupInfo struct {
	GroupID string `json:"groupId"`
	Type    string `json:"type"` // "UPDATE" when its members, name or description changed
}

// image is the attachment if it is an image the model can be shown, else
// its thumbnail if that is, else nil.
func (a *Attachment) image() *Attachment {
	for _, c := range []*Attachment{a, a.Thumbnail} {
		if c != nil && c.ID != "" && c.Size <= img.MaxBytes && img.Supported(c.ContentType) {
			return c
		}
	}
	return nil
}

// Sender is the sender's ID: an ACI, else a number.
func (e *Envelope) Sender() string { return cmp.Or(e.SourceUUID, e.Source, e.SourceNumber) }

// Chat is the chat env arrived in: its group, else a DM with its sender.
func (e *Envelope) Chat() feature.Chat {
	if d := e.DataMessage; d != nil && d.GroupInfo != nil && d.GroupInfo.GroupID != "" {
		return feature.Chat{GroupID: d.GroupInfo.GroupID}
	}
	return feature.Chat{User: e.Sender()}
}

// FromSelf reports whether the bot sent env.
func FromSelf(env *Envelope, self string) bool {
	return env.SourceUUID == self || env.Source == self || env.SourceNumber == self
}

// Allowed reports whether env's sender may DM the bot.
func Allowed(env *Envelope, allowed map[string]bool) bool {
	return allowed[env.SourceUUID] || allowed[env.Source] || allowed[env.SourceNumber]
}

// Addressed reports whether env is for the bot: it mentions the bot,
// replies to one of its messages, or is a DM from an allowed sender, and
// has text, an image or a sticker. It never blocks or allocates, so the
// reader goroutine can decide with it.
func Addressed(env *Envelope, self string, allowed map[string]bool) bool {
	d := env.DataMessage
	if d == nil || self == "" || FromSelf(env, self) {
		return false
	}
	if strings.TrimSpace(d.Message) == "" && d.Sticker == nil &&
		!slices.ContainsFunc(d.Attachments, func(a Attachment) bool { return a.image() != nil }) {
		return false
	}
	if d.GroupInfo == nil || d.GroupInfo.GroupID == "" {
		return Allowed(env, allowed)
	}
	for _, m := range d.Mentions {
		if m.UUID == self || m.Number == self {
			return true
		}
	}
	q := d.Quote
	return q != nil && (q.AuthorUUID == self || q.Author == self)
}

// Message is env as the core and features see it. self is the bot's ID.
func (c *Client) Message(env *Envelope, self string, allowed map[string]bool) feature.Message {
	d := cmp.Or(env.DataMessage, &DataMessage{})
	addressed := Addressed(env, self, allowed)
	m := feature.Message{
		Chat:      env.Chat(),
		Sender:    env.Sender(),
		Name:      txt.SafeName(env.SourceName),
		Text:      c.text(d.Message, d.Mentions, self, addressed),
		Expires:   time.Duration(d.ExpiresInSeconds) * time.Second,
		Addressed: addressed,
	}
	images := func(as []Attachment, quoted bool) {
		for i := range as {
			if a := as[i].image(); a != nil && len(m.Media) < feature.MaxPictures {
				m.Media = append(m.Media, feature.Media{ID: a.ID, Type: a.ContentType, Quoted: quoted})
			}
		}
	}
	images(d.Attachments, false)
	if q := d.Quote; q != nil {
		images(q.Attachments, true)
		m.Quote = &feature.Quote{Text: q.Text, Author: c.Name(q.AuthorUUID, q.Author), Bot: q.AuthorUUID == self || q.Author == self}
	}
	if s := d.Sticker; s != nil {
		m.Media = append(m.Media, feature.Media{Pack: s.PackID, Sticker: s.StickerID})
	}
	return m
}

// Person is a name the model was shown for someone in a group, so "@Name"
// in a reply can become a real mention of them.
type Person struct{ Name, ID string }

// People is the names a group message showed the model: its sender's and
// those of the people it mentions. A DM shows none.
func (c *Client) People(env *Envelope, self string) []Person {
	d := env.DataMessage
	if d == nil || d.GroupInfo == nil || d.GroupInfo.GroupID == "" {
		return nil
	}
	var out []Person
	add := func(name, id string) {
		if name = txt.SafeName(name); name != "" && id != "" && id != self {
			out = append(out, Person{name, id})
		}
	}
	add(env.SourceName, env.SourceUUID)
	for _, m := range d.Mentions {
		if m.Number != self {
			add(c.resolve(m), m.UUID)
		}
	}
	return out
}

// text removes the bot's own mention from msg and shows other mentions as
// @Name, or [unnamed user], logged only when logIt is set: an unaddressed
// message is never logged. Mentions are applied from the end, so earlier
// offsets stay valid; signal-cli's order isn't guaranteed, so they are
// sorted, and one that overlaps another or falls outside msg is skipped.
func (c *Client) text(msg string, ms []Mention, self string, logIt bool) string {
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
				if logIt {
					logx.Debugf("unresolved mention of %s", cmp.Or(m.UUID, m.Number))
				}
				name = unnamed
			}
			repl = utf16.Encode([]rune(name))
		}
		u = append(u[:m.Start], append(repl, u[m.Start+m.Length:]...)...)
		applied = m.Start
	}
	return strings.TrimSpace(string(utf16.Decode(u)))
}

// span is one "@name" in a text, as rune indices [start, end).
type span struct {
	start, end int
	id         string
}

// findAt finds "@name" for each name in known (name to ID). Matching
// ignores case, tries the longest name first, and sits on word boundaries.
func findAt(rs []rune, known map[string]string) []span {
	type cand struct {
		lower string
		n     int // runes
		id    string
	}
	var cands []cand
	for name, id := range known {
		if name != "" {
			cands = append(cands, cand{strings.ToLower(name), utf8.RuneCountInString(name), id})
		}
	}
	if len(cands) == 0 {
		return nil
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].n > cands[j].n })
	var out []span
	for i := 0; i < len(rs); i++ {
		if rs[i] != '@' || (i > 0 && isWord(rs[i-1])) {
			continue
		}
		for _, k := range cands {
			end := i + 1 + k.n
			if end > len(rs) || (end < len(rs) && isWord(rs[end])) || strings.ToLower(string(rs[i+1:end])) != k.lower {
				continue
			}
			out = append(out, span{i, end, k.id})
			i = end - 1
			break
		}
	}
	return out
}

func isWord(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' }

func utf16Len(rs []rune) (n int) {
	for _, r := range rs {
		n += utf16.RuneLen(r)
	}
	return n
}

// Mentions is the signal-cli mentions ("start:length:uuid", in UTF-16
// units) for each @Name in text whose name is in known (lower-cased name to
// ID). An ambiguous name, with no ID, stays plain text.
func Mentions(text string, known map[string]string) []string {
	rs := []rune(text)
	var out []string
	u, pos := 0, 0 // the UTF-16 offset of rs[pos]
	for _, s := range findAt(rs, known) {
		u += utf16Len(rs[pos:s.start])
		n := utf16Len(rs[s.start:s.end])
		if s.id != "" {
			out = append(out, fmt.Sprintf("%d:%d:%s", u, n, s.id))
		}
		u += n
		pos = s.end
	}
	return out
}

// StripAt removes "@name" for each of names from text, without leaving a
// double space, and reports whether it found one.
func StripAt(text string, names ...string) (string, bool) {
	known := map[string]string{} // findAt needs only the names
	for _, n := range names {
		known[txt.SafeName(n)] = ""
	}
	rs := []rune(text)
	spans := findAt(rs, known)
	if len(spans) == 0 {
		return text, false
	}
	var out []rune
	pos := 0
	for _, s := range spans {
		out = append(out, rs[pos:s.start]...)
		pos = s.end
		if pos < len(rs) && rs[pos] == ' ' && (len(out) == 0 || out[len(out)-1] == ' ') {
			pos++
		}
	}
	return strings.TrimSpace(string(append(out, rs[pos:]...))), true
}

// Parts turns a reply into the messages to send: without the bot's own
// @name, capped as a whole at MaxReply, and split at Signal's limit, preferring a line
// break and then a space.
func Parts(reply, self string) []string {
	reply, _ = StripAt(reply, self)
	r := []rune(reply)
	if len(r) > MaxReply {
		r = append(r[:MaxReply-utf8.RuneCountInString(truncated)], []rune(truncated)...)
	}
	var out []string
	for {
		for len(r) > 0 && unicode.IsSpace(r[0]) {
			r = r[1:]
		}
		if len(r) == 0 {
			return out
		}
		end := min(len(r), MaxChars)
		if end < len(r) {
			if i := lastIndex(r[:end], '\n'); i > 0 {
				end = i
			} else if i := lastIndex(r[:end], ' '); i > MaxChars/2 {
				end = i
			}
		}
		out = append(out, strings.TrimRightFunc(string(r[:end]), unicode.IsSpace))
		r = r[end:]
	}
}

func lastIndex(r []rune, want rune) int {
	for i := len(r) - 1; i >= 0; i-- {
		if r[i] == want {
			return i
		}
	}
	return -1
}

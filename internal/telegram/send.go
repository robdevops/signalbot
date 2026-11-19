package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/img"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/mdstyle"
)

const (
	maxChars   = 4096 // a message's limit, in UTF-16 units
	maxCaption = 1024 // a photo caption's
	// maxReply caps a whole reply, so one request can't fan out into many
	// messages.
	maxReply  = 8000
	truncated = " […truncated]"
)

// styleTypes are Telegram's entity types for the styles; code that spans
// lines is a "pre" block instead.
var styleTypes = map[mdstyle.Style]string{mdstyle.Bold: "bold", mdstyle.Italic: "italic", mdstyle.Strike: "strikethrough", mdstyle.Mono: "code", mdstyle.Spoiler: "spoiler"}

// outEntity is a MessageEntity the bot sends.
type outEntity struct {
	Type   string           `json:"type"`
	Offset int              `json:"offset"`
	Length int              `json:"length"`
	URL    string           `json:"url,omitempty"`
	User   map[string]int64 `json:"user,omitempty"`
}

// sentMsg is what a send returns: the message's ID.
type sentMsg struct {
	MessageID int64 `json:"message_id"`
}

// Send posts a reply to chat: its Markdown as Telegram's entities, a link
// under its own text, each @Name in known (lowercased name to ID) a text
// mention, split at Telegram's limit, the pictures first with the reply as
// their caption when it fits. It returns the IDs of the messages sent. A
// failure is logged.
func (c *Client) Send(ch feature.Chat, text string, pics []feature.Picture, known map[string]string) (sent []int64) {
	if n := utf8.RuneCountInString(text); n > maxReply {
		logx.Warnf("reply truncated from %d chars", n)
	}
	t := mdstyle.Parse(text, true)
	if sp := mdstyle.FindAt(t.Runes, map[string]string{c.usernameOf(): "", c.BotName(): ""}); len(sp) > 0 && strings.TrimSpace(string(t.Runes[:sp[0].Start])) == "" {
		t = t.Slice(sp[0].End, len(t.Runes)) // a leading echo of how the bot was addressed
	}
	t = t.Trim()
	if len(t.Runes) > maxReply {
		t = t.Slice(0, maxReply-utf8.RuneCountInString(truncated))
		t.Add([]rune(truncated), 0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	note := func(msgID int64) {
		sent = append(sent, msgID)
		c.noteSent(c.dest(ch).chat, msgID) // at a group's new ID, if the send found it moved
	}
	if len(pics) > 0 {
		var caption *mdstyle.Text
		if u := mdstyle.UTF16At(t.Runes); u[len(t.Runes)] <= maxCaption && len(t.Runes) > 0 {
			c := t
			caption = &c
		}
		ids, err := c.sendPhotos(ctx, ch, pics, caption, known)
		if err != nil {
			logx.Errorf("send failed: pictures: %v", err) // the text still goes
		} else if caption != nil {
			t = mdstyle.Text{}
		}
		for _, i := range ids {
			note(i)
		}
	}
	parts := mdstyle.Split(t, maxChars)
	if len(parts) == 0 && len(pics) == 0 {
		logx.Warnf("empty reply, nothing sent")
		return nil
	}
	for _, p := range parts {
		var m sentMsg
		d := c.dest(ch)
		params := d.params(map[string]any{"text": string(p.Runes), "entities": entities(p, known)})
		send := func() error { return c.call(ctx, "sendMessage", params, &m) }
		move := func(to string) { params["chat_id"] = to }
		err := c.retrying(ctx, d.chat, move, send)
		if refusedEntities(err) {
			delete(params, "entities") // Telegram refused them: the text alone
			err = c.retrying(ctx, d.chat, move, send)
		}
		if err != nil {
			logx.Errorf("send failed: %v", err)
			return sent
		}
		note(m.MessageID)
	}
	return sent
}

// refusedEntities reports whether err is Telegram refusing a message's
// entities, so it can go without them.
func refusedEntities(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Code == 400 && strings.Contains(strings.ToLower(ae.Desc), "entit")
}

// dest is where a chat's messages go: its chat_id, and in a forum
// supergroup the topic, message_thread_id.
type dest struct {
	chat   string
	thread int64
}

// dest is ch's destination: a DM's user, or a group's ID, with its topic
// after a slash if it has one, at its new ID if it became a supergroup.
func (c *Client) dest(ch feature.Chat) dest {
	if ch.GroupID == "" {
		return dest{chat: ch.User}
	}
	base, topic, _ := strings.Cut(ch.GroupID, "/")
	n, _ := strconv.ParseInt(topic, 10, 64)
	c.mu.Lock()
	if to, ok := c.moved[base]; ok {
		base = to
	}
	c.mu.Unlock()
	return dest{chat: base, thread: n}
}

// params is p with the destination's chat_id and topic.
func (d dest) params(p map[string]any) map[string]any {
	p["chat_id"] = d.chat
	if d.thread != 0 {
		p["message_thread_id"] = d.thread
	}
	return p
}

// fields is f, a multipart form's, with the destination's chat_id and topic.
func (d dest) fields(f map[string]string) map[string]string {
	f["chat_id"] = d.chat
	if d.thread != 0 {
		f["message_thread_id"] = strconv.FormatInt(d.thread, 10)
	}
	return f
}

// chatID is ch's Telegram chat, without a topic: message IDs are unique
// within it.
func chatID(ch feature.Chat) string {
	if ch.GroupID != "" {
		base, _, _ := strings.Cut(ch.GroupID, "/")
		return base
	}
	return ch.User
}

// entities are t's styles, links and mentions of known names as Telegram's
// entities, in UTF-16 units.
func entities(t mdstyle.Text, known map[string]string) []outEntity {
	u := mdstyle.UTF16At(t.Runes)
	var out []outEntity
	for _, r := range t.Runs() {
		typ := styleTypes[r.Style]
		if r.Style == mdstyle.Mono && strings.ContainsRune(string(utf16Slice(t.Runes, u, r.Start, r.Len)), '\n') {
			typ = "pre"
		}
		out = append(out, outEntity{Type: typ, Offset: r.Start, Length: r.Len})
	}
	for _, l := range t.Links {
		out = append(out, outEntity{Type: "text_link", Offset: u[l.Start], Length: u[l.End] - u[l.Start], URL: l.URL})
	}
	for _, s := range mdstyle.FindAt(t.Runes, known) {
		if n, err := strconv.ParseInt(s.ID, 10, 64); err == nil && n > 0 {
			out = append(out, outEntity{Type: "text_mention", Offset: u[s.Start], Length: u[s.End] - u[s.Start], User: map[string]int64{"id": n}})
		}
	}
	return out
}

// utf16Slice is the runes of rs covering UTF-16 units [start, start+n).
func utf16Slice(rs []rune, u []int, start, n int) []rune {
	i, j := 0, 0
	for i < len(rs) && u[i] < start {
		i++
	}
	for j = i; j < len(rs) && u[j] < start+n; j++ {
	}
	return rs[i:j]
}

// sendPhotos sends pics, one as a photo or several as an album, with
// caption on the first, without its entities if Telegram refuses them, and
// returns the IDs of the messages sent.
func (c *Client) sendPhotos(ctx context.Context, ch feature.Chat, pics []feature.Picture, caption *mdstyle.Text, known map[string]string) ([]int64, error) {
	pics = pics[:min(len(pics), 10)] // an album's limit
	files := map[string][]byte{}
	for i, p := range pics {
		files["p"+strconv.Itoa(i)] = p.Data
	}
	ents := []outEntity{}
	if caption != nil {
		ents = append(ents, entities(*caption, known)...)
	}
	ids, err := c.sendPhotosOnce(ctx, ch, pics, files, caption, ents)
	if caption != nil && len(ents) > 0 && refusedEntities(err) {
		ids, err = c.sendPhotosOnce(ctx, ch, pics, files, caption, []outEntity{})
	}
	return ids, err
}

func (c *Client) sendPhotosOnce(ctx context.Context, ch feature.Chat, pics []feature.Picture, files map[string][]byte, caption *mdstyle.Text, ents []outEntity) ([]int64, error) {
	text := ""
	if caption != nil {
		text = string(caption.Runes)
	}
	ej, _ := json.Marshal(ents)
	d := c.dest(ch)
	var fields map[string]string
	var out any
	var one sentMsg
	var album []sentMsg
	method := "sendPhoto"
	if len(pics) == 1 {
		fields, out = d.fields(map[string]string{"photo": "attach://p0", "caption": text, "caption_entities": string(ej)}), &one
	} else {
		media := make([]map[string]any, len(pics))
		for i := range pics {
			media[i] = map[string]any{"type": "photo", "media": "attach://p" + strconv.Itoa(i)}
		}
		if caption != nil {
			media[0]["caption"], media[0]["caption_entities"] = text, json.RawMessage(ej)
		}
		mj, _ := json.Marshal(media)
		method, fields, out = "sendMediaGroup", d.fields(map[string]string{"media": string(mj)}), &album
	}
	err := c.retrying(ctx, d.chat, func(to string) { fields["chat_id"] = to }, func() error { return c.upload(ctx, method, fields, files, out) })
	if err != nil {
		return nil, err
	}
	if len(pics) == 1 {
		return []int64{one.MessageID}, nil
	}
	var ids []int64
	for _, m := range album {
		ids = append(ids, m.MessageID)
	}
	return ids, nil
}

// React puts emoji on msg, as a reaction from the bot. Telegram allows only
// some emoji as reactions, so one it refuses as such is sent as a message
// instead.
func (c *Client) React(ch feature.Chat, msg feature.Message, emoji string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	d := c.dest(ch)
	err := c.call(ctx, "setMessageReaction", map[string]any{"chat_id": d.chat, "message_id": msg.Timestamp,
		"reaction": []map[string]string{{"type": "emoji", "emoji": emoji}}}, nil)
	var ae *apiError
	switch {
	case err == nil:
	case errors.As(err, &ae) && ae.Code == 400 && strings.Contains(ae.Desc, "REACTION_INVALID"):
		c.Send(ch, emoji, nil, nil)
	default:
		logx.Errorf("reaction failed: %v", err)
	}
}

// Typing shows the bot typing in chat until stop is called; Telegram's
// lasts 5s, so it is renewed.
func (c *Client) Typing(ch feature.Chat) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	send := func() {
		tctx, tcancel := context.WithTimeout(ctx, 10*time.Second)
		defer tcancel()
		if err := c.call(tctx, "sendChatAction", c.dest(ch).params(map[string]any{"action": "typing"}), nil); err != nil && ctx.Err() == nil {
			logx.Debugf("telegram typing failed: %v", err)
		}
	}
	go func() {
		send()
		tick := time.NewTicker(c.typing)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				send()
			}
		}
	}()
	return cancel
}

// Media fetches msg.Media[i] by its file ID: getFile, then a download,
// refusing one over the bot's cap, waiting at most 30s.
func (c *Client) Media(ctx context.Context, msg feature.Message, i int) ([]byte, error) {
	if i < 0 || i >= len(msg.Media) || msg.Media[i].ID == "" {
		return nil, errors.New("no such media")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var f struct {
		FilePath string `json:"file_path"`
		FileSize int    `json:"file_size"`
	}
	if err := c.call(ctx, "getFile", map[string]string{"file_id": msg.Media[i].ID}, &f); err != nil {
		return nil, err
	}
	if f.FilePath == "" || f.FileSize > img.MaxReceived {
		return nil, fmt.Errorf("file too big or gone (%d bytes)", f.FileSize)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/file/bot"+c.token+"/"+f.FilePath, nil)
	if err != nil {
		return nil, c.scrub(err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.scrub(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download: status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, img.MaxReceived+1))
	if err != nil {
		return nil, c.scrub(err)
	}
	if len(data) > img.MaxReceived {
		return nil, errors.New("file too big")
	}
	return data, nil
}

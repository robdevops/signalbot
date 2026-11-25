package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
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
// their caption when it fits, the first message a reply to the bot's own
// message replyTo if it isn't 0 (sent anyway if that message is gone). mk's
// buttons, or in a DM its keyboard, go on the last message, which can't be
// an album, and with Silent every message is sent without a notification.
// It returns the IDs of the messages sent. A failure is logged.
func (c *Client) Send(ch feature.Chat, text string, pics []feature.Picture, known map[string]string, replyTo int64, audio []byte, mk feature.Markup) (sent []int64) {
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
	note := func(msgID int64) { // only the first message sent replies
		sent, replyTo = append(sent, msgID), 0
		c.noteSent(c.dest(ch).chat, msgID) // at a group's new ID, if the send found it moved
	}
	if len(pics) > 0 {
		var caption *mdstyle.Text
		if u := mdstyle.UTF16At(t.Runes); u[len(t.Runes)] <= maxCaption && len(t.Runes) > 0 {
			c := t
			caption = &c
		}
		var last any // the markup, if nothing follows the photo
		if (caption != nil || len(t.Runes) == 0) && len(audio) == 0 {
			last = markup(ch, mk)
		}
		ids, err := c.sendPhotos(ctx, ch, pics, caption, known, replyTo, mk.Silent, last)
		if err != nil {
			logx.Errorf("send failed: pictures: %v", err) // the text still goes
		} else if caption != nil {
			t = mdstyle.Text{}
		}
		for _, i := range ids {
			note(i)
		}
	}
	if len(audio) > 0 { // a voice message: it shows no title or performer
		var last any // the markup, if no text follows the clip
		if len(t.Runes) == 0 {
			last = markup(ch, mk)
		}
		id, err := c.sendVoice(ctx, ch, audio, replyTo, mk.Silent, last)
		if err != nil {
			logx.Errorf("send failed: audio: %v", err) // the text still goes
		} else {
			note(id)
		}
	}
	parts := mdstyle.Split(t, maxChars)
	if len(parts) == 0 && len(pics) == 0 && len(audio) == 0 {
		logx.Warnf("empty reply, nothing sent")
		return nil
	}
	for i, p := range parts {
		var m sentMsg
		d := c.dest(ch)
		params := d.params(map[string]any{"text": string(p.Runes), "entities": entities(p, known)})
		if replyTo != 0 {
			params["reply_parameters"] = replyParams(replyTo)
		}
		if mk.Silent {
			params["disable_notification"] = true
		}
		if rm := markup(ch, mk); rm != nil && i == len(parts)-1 {
			params["reply_markup"] = rm
		}
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

// markup is mk as Telegram's reply_markup: its buttons as an inline
// keyboard, or in a DM its keyboard as a persistent reply keyboard, or that
// keyboard taken away; nil for none.
func markup(ch feature.Chat, mk feature.Markup) any {
	switch {
	case len(mk.Buttons) > 0:
		return inlineKeyboard(mk.Buttons)
	case ch.GroupID != "":
	case len(mk.Keyboard) > 0:
		rows := make([][]map[string]string, len(mk.Keyboard))
		for i, row := range mk.Keyboard {
			for _, k := range row {
				rows[i] = append(rows[i], map[string]string{"text": k.Label})
			}
		}
		return map[string]any{"keyboard": rows, "resize_keyboard": true, "is_persistent": true}
	case mk.NoKeyboard:
		return map[string]bool{"remove_keyboard": true}
	}
	return nil
}

// inlineKeyboard is buttons as Telegram's InlineKeyboardMarkup, each
// button's Data its callback_data, and a highlighted one in the "primary"
// style (blue).
func inlineKeyboard(buttons [][]feature.Button) map[string]any {
	rows := make([][]map[string]string, len(buttons))
	for i, row := range buttons {
		for _, b := range row {
			btn := map[string]string{"text": b.Label, "callback_data": b.Data}
			if b.Highlight {
				btn["style"] = "primary"
			}
			rows[i] = append(rows[i], btn)
		}
	}
	return map[string]any{"inline_keyboard": rows}
}

// replyParams are Telegram's reply_parameters for a reply to the message
// id in the same chat, sent without the reply if that message is gone.
func replyParams(id int64) map[string]any {
	return map[string]any{"message_id": id, "allow_sending_without_reply": true}
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
// caption on the first, without its entities if Telegram refuses them, as
// a reply to replyTo if it isn't 0, without a notification if silent, and
// with rm as one photo's reply_markup if not nil (an album takes none), and
// returns the IDs of the messages sent.
func (c *Client) sendPhotos(ctx context.Context, ch feature.Chat, pics []feature.Picture, caption *mdstyle.Text, known map[string]string, replyTo int64, silent bool, rm any) ([]int64, error) {
	pics = pics[:min(len(pics), 10)] // an album's limit
	files := map[string][]byte{}
	for i, p := range pics {
		files["p"+strconv.Itoa(i)] = p.Data
	}
	ents := []outEntity{}
	if caption != nil {
		ents = append(ents, entities(*caption, known)...)
	}
	extra := map[string]string{}
	if silent {
		extra["disable_notification"] = "true"
	}
	if rm != nil && len(pics) == 1 {
		j, _ := json.Marshal(rm)
		extra["reply_markup"] = string(j)
	}
	ids, err := c.sendPhotosOnce(ctx, ch, pics, files, caption, ents, replyTo, extra)
	if caption != nil && len(ents) > 0 && refusedEntities(err) {
		ids, err = c.sendPhotosOnce(ctx, ch, pics, files, caption, []outEntity{}, replyTo, extra)
	}
	return ids, err
}

func (c *Client) sendPhotosOnce(ctx context.Context, ch feature.Chat, pics []feature.Picture, files map[string][]byte, caption *mdstyle.Text, ents []outEntity, replyTo int64, extra map[string]string) ([]int64, error) {
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
	if replyTo != 0 {
		rj, _ := json.Marshal(replyParams(replyTo))
		fields["reply_parameters"] = string(rj)
	}
	maps.Copy(fields, extra)
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

// sendVoice sends audio, an MP3 clip, as a voice message, which plays in
// the chat and shows no title or performer, without a notification if
// silent and with rm as its reply_markup if not nil, and returns its
// message ID.
func (c *Client) sendVoice(ctx context.Context, ch feature.Chat, audio []byte, replyTo int64, silent bool, rm any) (int64, error) {
	d := c.dest(ch)
	fields := d.fields(map[string]string{})
	if replyTo != 0 {
		rj, _ := json.Marshal(replyParams(replyTo))
		fields["reply_parameters"] = string(rj)
	}
	if silent {
		fields["disable_notification"] = "true"
	}
	if rm != nil {
		j, _ := json.Marshal(rm)
		fields["reply_markup"] = string(j)
	}
	var m sentMsg
	err := c.retrying(ctx, d.chat, func(to string) { fields["chat_id"] = to }, func() error {
		return c.upload(ctx, "sendVoice", fields, map[string][]byte{"voice": audio}, &m)
	})
	return m.MessageID, err
}

// React puts emoji on msg, as a reaction from the bot: by its message ID,
// which the bot's own messages have too. Telegram allows only some emoji as
// reactions, so one it refuses as such is sent as a message instead.
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
		c.Send(ch, emoji, nil, nil, 0, nil, feature.Markup{})
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

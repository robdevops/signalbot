package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/mdstyle"
)

// callbackQuery is a press of a button under one of the bot's messages.
type callbackQuery struct {
	ID      string   `json:"id"`
	From    *user    `json:"from"`
	Message *message `json:"message"` // the message the button is under; only its chat and ID if it is old
	Data    string   `json:"data"`
}

// Buttons reports that Telegram shows a post's Markup.
func (c *Client) Caps() feature.Caps {
	return feature.Caps{Buttons: true, Keyboard: true, Silent: true, EditMedia: true, Caption: maxCaption}
}

// pressed is a press as the core gets it. A press it drops is answered at
// once, so the button stops spinning.
func (c *Client) pressed(q *callbackQuery) (bot.Received, bool) {
	answer := func(text string) { go c.answerPress(q.ID, text) }
	if q.From == nil || q.Message == nil || q.Data == "" || id(q.From.ID) == c.selfID() {
		answer("")
		return nil, false
	}
	ch := chatOf(q.Message)
	return bot.Pressed{Press: feature.Press{Chat: ch, By: c.person(q.From), Sent: q.Message.MessageID, Data: q.Data}, Answer: answer}, true
}

// maxToast caps what a press's answer shows, Telegram's limit, in UTF-16
// units.
const maxToast = 200

// toast is text cut to fit maxToast, with "…" if it was cut.
func toast(text string) string {
	rs := []rune(text)
	u := mdstyle.UTF16At(rs)
	if u[len(rs)] <= maxToast {
		return text
	}
	n := len(rs)
	for u[n] > maxToast-1 {
		n--
	}
	return string(rs[:n]) + "…"
}

// answerPress answers the press called queryID, showing text, if not "",
// for a moment.
func (c *Client) answerPress(queryID, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p := map[string]any{"callback_query_id": queryID}
	if text = strings.TrimSpace(text); text != "" {
		p["text"] = toast(text)
	}
	err := c.call(ctx, "answerCallbackQuery", p, nil)
	if _, ok := p["text"]; ok && refused(err) { // the text refused: the button must still stop spinning
		delete(p, "text")
		err = c.call(ctx, "answerCallbackQuery", p, nil)
	}
	if err != nil {
		logx.Warnf("telegram: answering a press failed: %v", err)
	}
}

// Edit replaces the bot's own message sent: with pics, its photo, the first
// of them (editMessageMedia), and its caption; without, its text, or for a
// message with a photo its caption. mk's buttons go under it; an edit
// without them takes them away, and one with no text and no pictures
// changes only them. Text past a message's or a caption's limit is cut.
// Telegram's "not modified" counts as done. A picture Telegram won't put
// on the message, such as one with none, goes in a new message, as on
// Signal. It returns sent, the new message's IDs, or none if the edit
// failed, which is logged.
func (c *Client) Edit(ch feature.Chat, sent int64, text string, pics []feature.Picture, known map[string]string, mk feature.Markup) []int64 {
	t := mdstyle.Parse(text, true).Trim()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	d := c.dest(ch)
	var rm any
	if len(mk.Buttons) > 0 {
		rm = inlineKeyboard(mk.Buttons)
	}
	cut := func(limit int) mdstyle.Text {
		if parts := mdstyle.Split(t, limit); len(parts) > 0 {
			return parts[0]
		}
		return mdstyle.Text{}
	}
	var err error
	switch {
	case len(pics) > 0:
		err = c.editMedia(ctx, d.chat, sent, pics[0].Data, cut(maxCaption), known, rm)
		if refused(err) && !notModified(err) {
			cancel()
			return c.Send(ch, text, pics[:1], known, 0, nil, feature.Markup{Buttons: mk.Buttons})
		}
	case len(t.Runes) > 0:
		params := map[string]any{"chat_id": d.chat, "message_id": sent, "text": string(cut(maxChars).Runes), "entities": entities(cut(maxChars), known)}
		if rm != nil {
			params["reply_markup"] = rm
		}
		err = c.editCall(ctx, d.chat, "editMessageText", params, "entities")
		if noText(err) { // a photo's message: its caption
			p := cut(maxCaption)
			delete(params, "text")
			delete(params, "entities")
			params["caption"], params["caption_entities"] = string(p.Runes), entities(p, known)
			err = c.editCall(ctx, d.chat, "editMessageCaption", params, "caption_entities")
		}
	default:
		params := map[string]any{"chat_id": d.chat, "message_id": sent}
		if rm != nil {
			params["reply_markup"] = rm
		}
		err = c.editCall(ctx, d.chat, "editMessageReplyMarkup", params, "")
	}
	if err != nil && !notModified(err) {
		logx.Errorf("edit failed: %v", err)
		return nil
	}
	return []int64{sent}
}

// editCall makes an edit with params, again without ents (its entities'
// key) if Telegram refuses them.
func (c *Client) editCall(ctx context.Context, chat, method string, params map[string]any, ents string) error {
	send := func() error { return c.call(ctx, method, params, nil) }
	move := func(to string) { params["chat_id"] = to }
	err := c.retrying(ctx, chat, move, send)
	if ents != "" && refusedEntities(err) {
		delete(params, ents)
		err = c.retrying(ctx, chat, move, send)
	}
	return err
}

// editMedia replaces message sent's photo with photo, captioned, and its
// buttons with rm, again without the caption's entities if Telegram
// refuses them.
func (c *Client) editMedia(ctx context.Context, chat string, sent int64, photo []byte, caption mdstyle.Text, known map[string]string, rm any) error {
	media := map[string]any{"type": "photo", "media": "attach://p0", "caption": string(caption.Runes)}
	if ents := entities(caption, known); len(ents) > 0 {
		media["caption_entities"] = ents
	}
	fields := map[string]string{"chat_id": chat, "message_id": strconv.FormatInt(sent, 10)}
	if rm != nil {
		j, _ := json.Marshal(rm)
		fields["reply_markup"] = string(j)
	}
	send := func() error {
		mj, _ := json.Marshal(media)
		fields["media"] = string(mj)
		return c.upload(ctx, "editMessageMedia", fields, map[string][]byte{"p0": photo}, nil)
	}
	move := func(to string) { fields["chat_id"] = to }
	err := c.retrying(ctx, chat, move, send)
	if refusedEntities(err) {
		delete(media, "caption_entities")
		err = c.retrying(ctx, chat, move, send)
	}
	return err
}

// refused reports whether err is Telegram refusing a request as bad.
func refused(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Code == 400
}

// noText reports whether err is Telegram refusing to edit the text of a
// message that has none, such as a photo's.
func noText(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Code == 400 && strings.Contains(strings.ToLower(ae.Desc), "no text")
}

// notModified reports whether err is Telegram saying an edit changes
// nothing.
func notModified(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Code == 400 && strings.Contains(strings.ToLower(ae.Desc), "not modified")
}

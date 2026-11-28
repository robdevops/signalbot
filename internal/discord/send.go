package discord

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/img"
	"github.com/robdevops/signalbot/internal/kit/mdstyle"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

const (
	maxChars = 2000 // a message's limit
	maxFiles = 10   // a message's attachments
	// maxReply caps a whole reply, so one request can't fan out into many
	// messages.
	maxReply  = 8000
	truncated = " […truncated]"
)

// part is one message's text in Discord's Markdown, and the users it
// mentions, whom it may notify.
type part struct {
	text  string
	users []string
}

// Send posts a reply to chat: its Markdown as Discord's, each @Name in
// known (name to ID) a mention, split at Discord's limit; the pictures, and
// the audio clip as an MP3 file, go first, with the reply when it fits in
// one message; the first message replies to the bot's own message replyTo
// if it isn't 0 (sent anyway if that message is gone). mk's buttons go on
// the last message, and with Silent every message is sent without a
// notification. While the core is answering a slash command (Answering),
// the first message replaces that command's "thinking…"; any other send
// in the chat is posted as usual. It returns the IDs of the messages sent.
// A failure is logged.
func (c *Client) Send(ch feature.Chat, text string, pics []feature.Picture, known map[string]string, replyTo int64, audio []byte, mk feature.Markup) (sent []int64) {
	return c.post(ch, text, pics, known, replyTo, audio, mk, c.take(ch))
}

// post is Send with the first message replacing ix's "thinking…" if ix
// isn't nil; if nothing is sent, the "thinking…" is taken away.
func (c *Client) post(ch feature.Chat, text string, pics []feature.Picture, known map[string]string, replyTo int64, audio []byte, mk feature.Markup, ix *interaction) (sent []int64) {
	defer func() {
		if ix != nil { // not used: set to nil once it is
			go c.unthink(ix)
		}
	}()
	if n := utf8.RuneCountInString(text); n > maxReply {
		log.Warnf("reply truncated from %d chars", n)
	}
	parts := c.parts(text, known, true)
	files := pictures(pics)
	if len(audio) > 0 && len(files) < maxFiles {
		files = append(files, upload{name: "clip.mp3", mime: "audio/mpeg", data: audio})
	}
	if len(parts) == 0 && len(files) == 0 {
		log.Warnf("empty reply, nothing sent")
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	chID, err := c.channel(ctx, ch)
	if err != nil {
		log.Errorf("send failed: %v", err)
		return nil
	}
	type msg struct {
		part
		files []upload
	}
	var msgs []msg
	if len(files) > 0 {
		m := msg{files: files}
		if len(parts) == 1 {
			m.part, parts = parts[0], nil
		}
		msgs = append(msgs, m)
	}
	for _, p := range parts {
		msgs = append(msgs, msg{part: p})
	}
	for i, m := range msgs {
		payload := map[string]any{"content": m.text, "allowed_mentions": allowed(m.users)}
		if i == len(msgs)-1 && len(mk.Buttons) > 0 {
			payload["components"] = components(mk.Buttons)
		}
		if len(m.files) > 0 {
			payload["attachments"] = attachments(m.files)
		}
		var out struct {
			ID string `json:"id"`
		}
		if ix != nil {
			ix.acknowledged()
			err = c.send(ctx, http.MethodPatch, "/webhooks/"+ix.app+"/"+ix.token+"/messages/@original", payload, m.files, &out)
			if err == nil {
				c.noteHook(snowflake(out.ID), ix)
			} else {
				log.Warnf("answering a slash command failed: %v", err)
				go c.unthink(ix)
			}
			ix = nil
		}
		if out.ID == "" {
			if replyTo != 0 {
				payload["message_reference"] = map[string]any{"message_id": id(replyTo), "fail_if_not_exists": false}
			}
			if mk.Silent {
				payload["flags"] = 4096
			}
			if err = c.send(ctx, http.MethodPost, "/channels/"+chID+"/messages", payload, m.files, &out); err != nil {
				log.Errorf("send failed: %v", err)
				return sent
			}
		}
		n := snowflake(out.ID)
		sent, replyTo = append(sent, n), 0 // only the first message replies
		c.mu.Lock()
		c.noteAuthor(n, prefix+c.selfID())
		c.mu.Unlock()
	}
	return sent
}

// parts is text as messages in Discord's Markdown: without a leading echo
// of how the bot was addressed if lead, and cut at maxReply.
func (c *Client) parts(text string, known map[string]string, lead bool) []part {
	t := mdstyle.Parse(text, true)
	if sp := mdstyle.FindAt(t.Runes, map[string]string{c.usernameOf(): "", c.BotName(): ""}); lead && len(sp) > 0 && strings.TrimSpace(string(t.Runes[:sp[0].Start])) == "" {
		t = t.Slice(sp[0].End, len(t.Runes))
	}
	t = t.Trim()
	if len(t.Runes) > maxReply {
		t = t.Slice(0, maxReply-utf8.RuneCountInString(truncated))
		t.Add([]rune(truncated), 0)
	}
	return split(t, known, maxChars-100)
}

// split cuts t into messages of at most maxChars once written as Discord's
// Markdown, cutting a part again, smaller, where its markup made it too
// long.
func split(t mdstyle.Text, known map[string]string, limit int) []part {
	var out []part
	for _, p := range mdstyle.Split(t, limit) {
		m := markdown(p, known)
		if len(utf16.Encode([]rune(m.text))) > maxChars && limit > 200 {
			out = append(out, split(p, known, limit/2)...)
			continue
		}
		out = append(out, m)
	}
	return out
}

// markers are Discord's Markdown for each style but code, in Styles' order.
var markers = map[mdstyle.Style]string{mdstyle.Bold: "**", mdstyle.Italic: "*", mdstyle.Strike: "~~", mdstyle.Spoiler: "||"}

// markdown is t in Discord's Markdown: its styles, code and links, each
// @Name in known a mention, and the rest escaped, but bare links, which
// Discord shows as they are.
func markdown(t mdstyle.Text, known map[string]string) part {
	var p part
	rs := t.Runes
	at := map[int]mdstyle.Span{}
	for _, s := range mdstyle.FindAt(rs, known) {
		if uid, ok := strings.CutPrefix(s.ID, prefix); ok && snowflake(uid) != 0 {
			at[s.Start] = s
		}
	}
	starts, ends := map[int]string{}, map[int]string{}
	for _, l := range t.Links {
		starts[l.Start], ends[l.End] = l.URL, l.URL
	}
	var b strings.Builder
	var open []mdstyle.Style
	set := func(want mdstyle.Style) { // close what is off, then open what is on
		k := 0
		for k < len(open) && want&open[k] != 0 {
			k++
		}
		for j := len(open) - 1; j >= k; j-- {
			b.WriteString(markers[open[j]])
		}
		open = open[:k]
		for _, st := range mdstyle.Styles {
			if st != mdstyle.Mono && want&st != 0 && !slices.Contains(open, st) {
				b.WriteString(markers[st])
				open = append(open, st)
			}
		}
	}
	inURL := false
	for i := 0; i < len(rs); {
		if u, ok := ends[i]; ok {
			set(0)
			b.WriteString("](" + u + ")")
		}
		if _, ok := starts[i]; ok {
			set(0)
			b.WriteString("[")
		}
		st := t.Styles[i]
		if st&mdstyle.Mono != 0 {
			j := i + 1
			for j < len(rs) && t.Styles[j]&mdstyle.Mono != 0 && starts[j] == "" && ends[j] == "" {
				j++
			}
			code := string(rs[i:j])
			if strings.Contains(code, "\n") {
				set(0)
				b.WriteString("```\n" + strings.ReplaceAll(code, "```", "`​``") + "\n```")
			} else {
				set(st &^ mdstyle.Mono)
				if strings.Contains(code, "`") {
					b.WriteString("`` " + code + " ``")
				} else {
					b.WriteString("`" + code + "`")
				}
			}
			i = j
			continue
		}
		set(st)
		if s, ok := at[i]; ok {
			uid := strings.TrimPrefix(s.ID, prefix)
			b.WriteString("<@" + uid + ">")
			p.users = append(p.users, uid)
			i = s.End
			continue
		}
		r := rs[i]
		if unicode.IsSpace(r) {
			inURL = false
		} else if !inURL && (i == 0 || !mdstyle.WordRune(rs[i-1])) && (hasPrefix(rs[i:], "https://") || hasPrefix(rs[i:], "http://")) {
			inURL = true
		}
		if !inURL && strings.ContainsRune(`\*_~`+"`"+`|`, r) {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
		i++
	}
	if u, ok := ends[len(rs)]; ok {
		set(0)
		b.WriteString("](" + u + ")")
	}
	set(0)
	p.text = b.String()
	return p
}

func hasPrefix(rs []rune, s string) bool {
	return len(rs) >= len(s) && strings.EqualFold(string(rs[:len(s)]), s)
}

// pictures is pics as files, at most maxFiles, each named after its Name or
// else pictureN, with the extension and type its data has.
func pictures(pics []feature.Picture) []upload {
	var files []upload
	for i, p := range pics[:min(len(pics), maxFiles)] {
		mime, ext := img.Type(p.Data)
		files = append(files, upload{name: fileName(cmp.Or(p.Name, "picture"+strconv.Itoa(i+1))) + "." + ext, mime: mime, data: p.Data})
	}
	return files
}

// fileName is name made safe as a file's: letters, digits, - and _ kept,
// the rest _.
func fileName(name string) string {
	return txt.Clamp(strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, name), 60)
}

// allowed is a message's allowed_mentions: only the users its text
// mentions are notified, never @everyone or a role.
func allowed(users []string) map[string]any {
	return map[string]any{"parse": []string{}, "users": append([]string{}, users...)}
}

// attachments lists files for a message's payload_json, by their index.
func attachments(files []upload) []map[string]any {
	out := make([]map[string]any, len(files))
	for i, f := range files {
		out[i] = map[string]any{"id": i, "filename": f.name}
	}
	return out
}

// components is buttons as Discord's action rows, at most 5 of 5, each
// button's Data its custom_id, and a highlighted one primary (blurple)
// where the rest are secondary (grey). More than 5 rows are packed again,
// in order, 5 buttons to a row; buttons past 25 are dropped, with a
// warning.
func components(buttons [][]feature.Button) []map[string]any {
	if len(buttons) > 5 {
		var all []feature.Button
		for _, row := range buttons {
			all = append(all, row...)
		}
		if len(all) > 25 {
			log.Warnf("%d buttons, only the first 25 shown", len(all))
			all = all[:25]
		}
		buttons = nil
		for len(all) > 0 {
			n := min(len(all), 5)
			buttons, all = append(buttons, all[:n]), all[n:]
		}
	}
	rows := []map[string]any{}
	for _, row := range buttons[:min(len(buttons), 5)] {
		var bs []map[string]any
		for _, b := range row[:min(len(row), 5)] {
			style := 2
			if b.Highlight {
				style = 1
			}
			bs = append(bs, map[string]any{"type": 2, "style": style, "label": txt.Clamp(b.Label, 79), "custom_id": b.Data})
		}
		if len(bs) > 0 {
			rows = append(rows, map[string]any{"type": 1, "components": bs})
		}
	}
	return rows
}

// channel is the Discord channel chat's messages go to: a DM's is opened,
// once, if the bot hasn't seen it yet.
func (c *Client) channel(ctx context.Context, ch feature.Chat) (string, error) {
	if g, ok := strings.CutPrefix(ch.GroupID, prefix); ok {
		return g, nil
	}
	uid, ok := strings.CutPrefix(ch.User, prefix)
	if !ok || ch.GroupID != "" {
		return "", errors.New("not a Discord chat")
	}
	c.mu.Lock()
	dm := c.dms[uid]
	c.mu.Unlock()
	if dm != "" {
		return dm, nil
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := c.call(ctx, http.MethodPost, "/users/@me/channels", map[string]string{"recipient_id": uid}, &out); err != nil {
		return "", fmt.Errorf("opening a DM: %w", err)
	}
	c.mu.Lock()
	if len(c.dms) < 10000 {
		c.dms[uid], c.dmUsers[out.ID] = out.ID, uid
	}
	c.mu.Unlock()
	return out.ID, nil
}

// Edit replaces the bot's own message sent with text, its first part if
// longer, and with pics, its attachments; mk's buttons go under it, and an
// edit without them takes them away. Text left empty with no pictures
// keeps the message's text. A slash command's reply is edited with its
// interaction's token while that works. It returns sent, or none if the
// edit failed, which is logged.
func (c *Client) Edit(ch feature.Chat, sent int64, text string, pics []feature.Picture, known map[string]string, mk feature.Markup) []int64 {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	chID, err := c.channel(ctx, ch)
	if err != nil {
		log.Errorf("edit failed: %v", err)
		return nil
	}
	payload := map[string]any{"components": components(mk.Buttons)}
	if parts := c.parts(text, known, false); len(parts) > 0 || len(pics) > 0 {
		p := part{}
		if len(parts) > 0 {
			p = parts[0]
		}
		payload["content"], payload["allowed_mentions"] = p.text, allowed(p.users)
	}
	files := pictures(pics)
	if len(files) > 0 {
		payload["attachments"] = attachments(files) // only these: the old ones go
	}
	path := "/channels/" + chID + "/messages/" + id(sent)
	if ix := c.hook(sent); ix != nil {
		path = "/webhooks/" + ix.app + "/" + ix.token + "/messages/" + id(sent)
	}
	if err := c.send(ctx, http.MethodPatch, path, payload, files, nil); err != nil {
		log.Errorf("edit failed: %v", err)
		return nil
	}
	return []int64{sent}
}

// React puts emoji on msg, as a reaction from the bot, and says whether it
// went on, or Discord refused the emoji (10014). A slash command, which is
// no message, gets it as its reply instead (which counts as on); a refused
// emoji sends nothing, and any other failure is logged.
func (c *Client) React(ch feature.Chat, msg feature.Message, emoji string) (on, refused bool) {
	if ix, found := c.takeID(ch, msg.Timestamp); found {
		return len(c.post(ch, emoji, nil, nil, 0, nil, feature.Markup{}, ix)) > 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	chID, err := c.channel(ctx, ch)
	if err == nil {
		err = c.call(ctx, http.MethodPut, "/channels/"+chID+"/messages/"+id(msg.Timestamp)+"/reactions/"+url.PathEscape(emoji)+"/@me", nil, nil)
	}
	var ae *apiError
	switch {
	case err == nil:
		return true, false
	case errors.As(err, &ae) && ae.Code == 10014: // unknown emoji
		log.Debugf("reaction refused: Discord doesn't know that emoji")
		return false, true
	default:
		log.Errorf("reaction failed: %v", err)
	}
	return false, false
}

// Typing shows the bot typing in chat until stop is called; Discord's
// lasts 10s, so it is renewed.
func (c *Client) Typing(ch feature.Chat) (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	send := func() {
		tctx, tcancel := context.WithTimeout(ctx, 10*time.Second)
		defer tcancel()
		chID, err := c.channel(tctx, ch)
		if err == nil {
			err = c.call(tctx, http.MethodPost, "/channels/"+chID+"/typing", nil, nil)
		}
		if err != nil && ctx.Err() == nil {
			log.Debugf("typing failed: %v", err)
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

// Media fetches msg.Media[i] from Discord's CDN, refusing one over the
// bot's cap, waiting at most 30s. The CDN's links are signed, so the bot's
// token isn't sent.
func (c *Client) Media(ctx context.Context, msg feature.Message, i int) ([]byte, error) {
	if i < 0 || i >= len(msg.Media) || msg.Media[i].ID == "" {
		return nil, errors.New("no such media")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, msg.Media[i].ID, nil)
	if err != nil {
		return nil, errors.New("download: bad link")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // without the link
		}
		return nil, fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download: status %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, img.MaxReceived+1))
	if err != nil {
		return nil, fmt.Errorf("download: %w", err)
	}
	if len(data) > img.MaxReceived {
		return nil, errors.New("file too big")
	}
	return data, nil
}

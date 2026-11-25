package slack

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/mdstyle"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

const (
	maxSection = 3000 // a section block's text, in characters
	maxBlocks  = 50   // a message's blocks
	// maxReply caps a whole reply, so one request can't fan out into many
	// messages.
	maxReply  = 8000
	truncated = " […truncated]"
)

// channelOf is the Slack channel chat's messages go to, and in a thread
// its parent's ts; for a DM, the DM channel, if the client knows it.
func (c *Client) channelOf(chat feature.Chat) (channel, thread string) {
	if chat.GroupID != "" {
		channel, thread, _ = strings.Cut(strings.TrimPrefix(chat.GroupID, prefix), "/")
		return channel, thread
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ims[strings.TrimPrefix(chat.User, prefix)], ""
}

// dest is chat's channel and thread, opening a DM by conversations.open if
// the client doesn't know its channel yet.
func (c *Client) dest(ctx context.Context, chat feature.Chat) (channel, thread string, err error) {
	if channel, thread = c.channelOf(chat); channel != "" || chat.GroupID != "" {
		return channel, thread, nil
	}
	user := strings.TrimPrefix(chat.User, prefix)
	var r struct {
		Channel struct {
			ID string `json:"id"`
		} `json:"channel"`
	}
	if err := c.call(ctx, "conversations.open", map[string]string{"users": user}, &r); err != nil {
		return "", "", err
	}
	if r.Channel.ID == "" {
		return "", "", errors.New("slack: conversations.open: no channel")
	}
	c.mu.Lock()
	c.noteIM(user, r.Channel.ID)
	c.mu.Unlock()
	return r.Channel.ID, "", nil
}

// Send posts a reply to chat: its Markdown as Slack's mrkdwn in section
// blocks, each @Name in known (lowercased name to ID) a mention, the
// pictures as image blocks above the text, mk's buttons under it, split
// into more messages past a message's blocks. In a thread it goes in the
// thread. Slack has no replies outside threads, each a chat of its own, so
// replyTo is not used, nor mk.Silent: a bot's message can't be silent. audio
// is shared as a file after the text. It returns the IDs of the messages
// sent. A failure is logged.
func (c *Client) Send(ch feature.Chat, text string, pics []feature.Picture, known map[string]string, replyTo int64, audio []byte, mk feature.Markup) (sent []int64) {
	if n := utf8.RuneCountInString(text); n > maxReply {
		logx.Warnf("reply truncated from %d chars", n)
	}
	t := c.trim(mdstyle.Parse(text, true))
	if len(t.Runes) > maxReply {
		t = t.Slice(0, maxReply-utf8.RuneCountInString(truncated))
		t.Add([]rune(truncated), 0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	channel, thread, err := c.dest(ctx, ch)
	if err != nil {
		logx.Errorf("send failed: %v", err)
		return nil
	}
	images := c.uploadPictures(ctx, pics)
	sections := sections(t, known)
	if len(sections) == 0 && len(images) == 0 && len(audio) == 0 {
		logx.Warnf("empty reply, nothing sent")
		return nil
	}
	buttons := actions(mk.Buttons)
	var msgs [][]any
	for _, b := range append(images, sections...) {
		if len(msgs) == 0 || len(msgs[len(msgs)-1]) >= maxBlocks {
			msgs = append(msgs, nil)
		}
		msgs[len(msgs)-1] = append(msgs[len(msgs)-1], b)
	}
	if len(buttons) > 0 && len(msgs) > 0 && len(msgs[len(msgs)-1])+len(buttons) <= maxBlocks {
		msgs[len(msgs)-1] = append(msgs[len(msgs)-1], buttons...)
	} else if len(buttons) > 0 {
		msgs = append(msgs, append([]any{section("·")}, buttons...))
	}
	for _, blocks := range msgs {
		ts, err := c.postBlocks(ctx, channel, thread, blocks)
		if err != nil {
			logx.Errorf("send failed: %v", err)
			return sent
		}
		sent = append(sent, tsInt(ts))
	}
	if len(audio) > 0 {
		if err := c.shareAudio(ctx, channel, thread, audio); err != nil {
			logx.Errorf("send failed: audio: %v", err)
		}
	}
	return sent
}

// trim is t without a leading echo of how the bot was addressed, and
// without white space at either end.
func (c *Client) trim(t mdstyle.Text) mdstyle.Text {
	if sp := mdstyle.FindAt(t.Runes, map[string]string{c.handleOf(): "", c.BotName(): ""}); len(sp) > 0 && strings.TrimSpace(string(t.Runes[:sp[0].Start])) == "" {
		t = t.Slice(sp[0].End, len(t.Runes))
	}
	return t.Trim()
}

// postBlocks posts blocks to channel, in thread if not "", by chat.postMessage,
// and returns the message's ts. A picture Slack is still processing makes
// it refuse the blocks: it is tried again after a moment, twice, then sent
// without its pictures.
func (c *Client) postBlocks(ctx context.Context, channel, thread string, blocks []any) (string, error) {
	params := map[string]any{"channel": channel, "text": fallback(blocks), "blocks": blocks, "unfurl_links": false}
	if thread != "" {
		params["thread_ts"] = thread
	}
	var r struct {
		TS string `json:"ts"`
	}
	err := c.call(ctx, "chat.postMessage", params, &r)
	for i := 0; i < 2 && code(err) == "invalid_blocks" && hasImage(blocks); i++ {
		if !sleep(ctx, c.settle) {
			break
		}
		err = c.call(ctx, "chat.postMessage", params, &r)
	}
	if code(err) == "invalid_blocks" && hasImage(blocks) {
		logx.Errorf("send failed: pictures: Slack refused them") // the text still goes
		blocks = withoutImages(blocks)
		if len(blocks) == 0 {
			return "", err
		}
		params["blocks"], params["text"] = blocks, fallback(blocks)
		err = c.call(ctx, "chat.postMessage", params, &r)
	}
	if err != nil {
		return "", err
	}
	c.noteSent(channel, r.TS)
	c.keepBlocks(channel, r.TS, blocks)
	return r.TS, nil
}

// fallback is the text a message's notification shows: its sections'.
func fallback(blocks []any) string {
	var parts []string
	for _, b := range blocks {
		if m, ok := b.(map[string]any); ok && m["type"] == "section" {
			if t, ok := m["text"].(map[string]string); ok {
				parts = append(parts, t["text"])
			}
		}
	}
	if len(parts) == 0 {
		return "image"
	}
	return txt.Clamp(strings.Join(parts, "\n"), maxSection)
}

func hasImage(blocks []any) bool {
	for _, b := range blocks {
		if m, ok := b.(map[string]any); ok && m["type"] == "image" {
			return true
		}
	}
	return false
}

func withoutImages(blocks []any) []any {
	var out []any
	for _, b := range blocks {
		if m, ok := b.(map[string]any); !ok || m["type"] != "image" {
			out = append(out, b)
		}
	}
	return out
}

func section(mrkdwn string) map[string]any {
	return map[string]any{"type": "section", "text": map[string]string{"type": "mrkdwn", "text": mrkdwn}}
}

// sections are t as section blocks of mrkdwn, each within a section's
// limit.
func sections(t mdstyle.Text, known map[string]string) []any {
	var out []any
	var add func(t mdstyle.Text, limit int)
	add = func(t mdstyle.Text, limit int) {
		for _, p := range mdstyle.Split(t, limit) {
			if s := mrkdwn(p, known); utf8.RuneCountInString(s) <= maxSection || limit < 200 {
				out = append(out, section(s))
			} else {
				add(p, limit/2) // its markup made it too long
			}
		}
	}
	add(t, maxSection*5/6)
	return out
}

// mrkdwn is t in Slack's mrkdwn: *bold*, _italic_, ~strike~, `code`, a
// block of code in ```, <url|text> for a link, <@U…> for a known name, and
// &, < and > escaped. A style's markers go inside the white space at its
// ends, as Slack wants them next to the text.
func mrkdwn(t mdstyle.Text, known map[string]string) string {
	links := map[int]mdstyle.Link{}
	for _, l := range t.Links {
		links[l.Start] = l
	}
	at := map[int]mdstyle.Span{}
	for _, s := range mdstyle.FindAt(t.Runes, known) {
		if id, ok := strings.CutPrefix(s.ID, prefix); ok && id != "" {
			s.ID = id
			at[s.Start] = s
		}
	}
	var b strings.Builder
	styled := func(rs []rune, st mdstyle.Style, inner string) {
		s := string(rs)
		core := strings.TrimFunc(s, unicode.IsSpace)
		if core == "" {
			b.WriteString(s)
			return
		}
		lead := s[:strings.Index(s, core)]
		trail := s[len(lead)+len(core):]
		if inner == "" {
			inner = escape(core)
		}
		open, close := markers(st, strings.Contains(core, "\n"))
		b.WriteString(lead + open + inner + close + trail)
	}
	for i := 0; i < len(t.Runes); {
		if l, ok := links[i]; ok && l.End > i {
			label := strings.NewReplacer("|", "¦").Replace(escape(string(t.Runes[i:l.End])))
			styled(t.Runes[i:l.End], t.Styles[i]&^mdstyle.Mono, "<"+escapeURL(l.URL)+"|"+label+">")
			i = l.End
			continue
		}
		if s, ok := at[i]; ok {
			b.WriteString("<@" + s.ID + ">")
			i = s.End
			continue
		}
		j := i + 1
		for j < len(t.Runes) && t.Styles[j] == t.Styles[i] {
			if _, ok := links[j]; ok {
				break
			}
			if _, ok := at[j]; ok {
				break
			}
			j++
		}
		styled(t.Runes[i:j], t.Styles[i], "")
		i = j
	}
	return b.String()
}

// markers are the mrkdwn markers that open and close st; code is shown
// alone, as Slack shows no style inside it, and a block of it if it spans
// lines.
func markers(st mdstyle.Style, lines bool) (open, close string) {
	if st&mdstyle.Mono != 0 {
		if lines {
			return "```\n", "\n```"
		}
		return "`", "`"
	}
	for _, m := range []struct {
		st mdstyle.Style
		m  string
	}{{mdstyle.Bold, "*"}, {mdstyle.Italic, "_"}, {mdstyle.Strike, "~"}} {
		if st&m.st != 0 {
			open, close = open+m.m, m.m+close
		}
	}
	return open, close
}

// escape is s with the three characters Slack's text escapes.
func escape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// escapeURL is u safe inside <…|…>.
func escapeURL(u string) string {
	return strings.NewReplacer("<", "%3C", ">", "%3E", "|", "%7C", "&", "&amp;").Replace(u)
}

// actions are buttons as Slack's actions blocks, a row each, at most 5 of
// 5 buttons; a highlighted one is in the "primary" style (green).
func actions(buttons [][]feature.Button) []any {
	var out []any
	for i, row := range buttons {
		if i >= 5 {
			break
		}
		var els []any
		for j, b := range row {
			if j >= 5 {
				break
			}
			el := map[string]any{"type": "button", "text": map[string]any{"type": "plain_text", "text": txt.Clamp(cmp.Or(b.Label, "·"), 75), "emoji": true},
				"action_id": "b" + strconv.Itoa(i) + "_" + strconv.Itoa(j), "value": b.Data}
			if b.Highlight {
				el["style"] = "primary"
			}
			els = append(els, el)
		}
		if len(els) > 0 {
			out = append(out, map[string]any{"type": "actions", "block_id": "row" + strconv.Itoa(i), "elements": els})
		}
	}
	return out
}

// upload puts data on Slack as a file named name, titled title, and
// returns its ID: files.getUploadURLExternal, the bytes to the URL it
// gives, then files.completeUploadExternal, shared in channel if not "".
func (c *Client) upload(ctx context.Context, name, title string, data []byte, channel, thread string) (string, error) {
	var u struct {
		URL    string `json:"upload_url"`
		FileID string `json:"file_id"`
	}
	if err := c.form(ctx, "files.getUploadURLExternal", c.token, url.Values{"filename": {name}, "length": {strconv.Itoa(len(data))}}, &u); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.URL, bytes.NewReader(data))
	if err != nil {
		return "", errors.New("upload: bad URL")
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.http.Do(req)
	if err != nil {
		return "", errors.New("upload failed") // the error quotes the URL
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("upload: status %d", resp.StatusCode)
	}
	params := map[string]any{"files": []map[string]string{{"id": u.FileID, "title": title}}}
	if channel != "" {
		params["channel_id"] = channel
		if thread != "" {
			params["thread_ts"] = thread
		}
	}
	return u.FileID, c.call(ctx, "files.completeUploadExternal", params, nil)
}

// uploadPictures uploads pics, at most 10, and returns them as image
// blocks; one that fails is logged and left out.
func (c *Client) uploadPictures(ctx context.Context, pics []feature.Picture) []any {
	var out []any
	for i, p := range pics {
		if i >= 10 {
			break
		}
		name := cmp.Or(p.Name, "image")
		id, err := c.upload(ctx, name+".png", name, p.Data, "", "")
		if err != nil {
			logx.Errorf("send failed: picture: %v", err)
			continue
		}
		out = append(out, map[string]any{"type": "image", "slack_file": map[string]string{"id": id}, "alt_text": txt.Clamp(name, 2000)})
	}
	return out
}

// shareAudio shares audio, an MP3 clip, in channel as a file.
func (c *Client) shareAudio(ctx context.Context, channel, thread string, audio []byte) error {
	_, err := c.upload(ctx, "audio.mp3", "audio", audio, channel, thread)
	return err
}

// keepBlocks remembers what the bot put in a message, but its buttons, so
// an edit that changes only the buttons can keep the rest; the last 500.
func (c *Client) keepBlocks(channel, ts string, blocks []any) {
	var kept []any
	for _, b := range blocks {
		if m, ok := b.(map[string]any); !ok || m["type"] != "actions" {
			kept = append(kept, b)
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	k := channel + "/" + ts
	if _, ok := c.blocks[k]; !ok {
		if c.blockQ = append(c.blockQ, k); len(c.blockQ) > 500 {
			delete(c.blocks, c.blockQ[0])
			c.blockQ = c.blockQ[1:]
		}
	}
	c.blocks[k] = kept
}

// Edit replaces the bot's own message sent, by chat.update: its pictures
// with pics, if any, its text with text, cut to one message's, and its
// buttons with mk's; an edit with no text and no pictures changes only the
// buttons, keeping what the bot last put in it (if it doesn't remember,
// the edit fails). It returns sent, or none if the edit failed, which is
// logged.
func (c *Client) Edit(ch feature.Chat, sent int64, text string, pics []feature.Picture, known map[string]string, mk feature.Markup) []int64 {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	channel, _, err := c.dest(ctx, ch)
	if err != nil {
		logx.Errorf("edit failed: %v", err)
		return nil
	}
	ts := tsString(sent)
	t := c.trim(mdstyle.Parse(text, true))
	var blocks []any
	if len(pics) == 0 && len(t.Runes) == 0 {
		c.mu.Lock()
		kept, ok := c.blocks[channel+"/"+ts]
		c.mu.Unlock()
		if !ok {
			logx.Errorf("edit failed: the message's content is not known")
			return nil
		}
		blocks = append(blocks, kept...)
	} else {
		blocks = c.uploadPictures(ctx, pics)
		secs := sections(t, known)
		blocks = append(blocks, secs[:min(len(secs), maxBlocks-len(blocks)-len(mk.Buttons))]...)
	}
	blocks = append(blocks, actions(mk.Buttons)...)
	params := map[string]any{"channel": channel, "ts": ts, "text": fallback(blocks), "blocks": blocks}
	err = c.call(ctx, "chat.update", params, nil)
	for i := 0; i < 2 && code(err) == "invalid_blocks" && hasImage(blocks); i++ {
		if !sleep(ctx, c.settle) {
			break
		}
		err = c.call(ctx, "chat.update", params, nil)
	}
	if err != nil {
		logx.Errorf("edit failed: %v", err)
		return nil
	}
	c.keepBlocks(channel, ts, blocks)
	return []int64{sent}
}

// React puts emoji on msg, as a reaction from the bot, by Slack's name for
// it; an emoji the table doesn't know is sent as a message instead.
func (c *Client) React(ch feature.Chat, msg feature.Message, emoji string) {
	name := nameOfEmoji(emoji)
	if name == "" {
		c.Send(ch, emoji, nil, nil, 0, nil, feature.Markup{})
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	channel, _, err := c.dest(ctx, ch)
	if err == nil {
		err = c.call(ctx, "reactions.add", map[string]string{"channel": channel, "timestamp": tsString(msg.Timestamp), "name": name}, nil)
	}
	if err != nil && code(err) != "already_reacted" {
		logx.Errorf("reaction failed: %v", err)
	}
}

// Typing does nothing: Slack shows no bot typing.
func (c *Client) Typing(feature.Chat) (stop func()) { return func() {} }

// ephemeral shows text to user alone in channel, in thread if not "": a
// press's answer.
func (c *Client) ephemeral(channel, thread, user, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p := map[string]any{"channel": channel, "user": user, "text": escape(txt.Clamp(text, maxSection))}
	if thread != "" {
		p["thread_ts"] = thread
	}
	if err := c.call(ctx, "chat.postEphemeral", p, nil); err != nil {
		logx.Warnf("slack: answering a press failed: %v", err)
	}
}

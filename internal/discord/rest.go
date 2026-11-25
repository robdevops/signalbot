package discord

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/feature"
)

// scrub is err with the token taken out of its text, should any error
// quote it.
func (c *Client) scrub(err error) error {
	if err == nil || c.token == "" || !strings.Contains(err.Error(), c.token) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), c.token, "<token>"))
}

// apiError is a refusal from the REST API.
type apiError struct {
	Status  int    // the HTTP status
	Code    int    // Discord's own error code, such as 10008 for an unknown message
	Message string // Discord's message
}

func (e *apiError) Error() string {
	return fmt.Sprintf("discord: %d %s (%d)", e.Status, e.Message, e.Code)
}

// status reports whether err is a refusal with the HTTP status code.
func status(err error, code int) bool {
	var ae *apiError
	return errors.As(err, &ae) && ae.Status == code
}

// call sends body, if not nil, as JSON to path by method, and decodes the
// answer into out, if not nil.
func (c *Client) call(ctx context.Context, method, path string, body, out any) error {
	var data []byte
	if body != nil {
		var err error
		if data, err = json.Marshal(body); err != nil {
			return err
		}
	}
	return c.do(ctx, method, path, "application/json", data, out)
}

// upload is a file sent with a message, of type mime, or
// application/octet-stream if that is "".
type upload struct {
	name, mime string
	data       []byte
}

// quoteEscaper escapes a file name in a quoted string, as multipart's
// CreateFormFile does.
var quoteEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

// send sends payload, a message's JSON, with files, if any, as a multipart
// form: Discord's payload_json and files[n].
func (c *Client) send(ctx context.Context, method, path string, payload map[string]any, files []upload, out any) error {
	if len(files) == 0 {
		return c.call(ctx, method, path, payload, out)
	}
	pj, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	w.WriteField("payload_json", string(pj))
	for i, f := range files {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="files[%d]"; filename="%s"`, i, quoteEscaper.Replace(f.name)))
		h.Set("Content-Type", cmp.Or(f.mime, "application/octet-stream"))
		fw, err := w.CreatePart(h)
		if err != nil {
			return err
		}
		fw.Write(f.data)
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.do(ctx, method, path, w.FormDataContentType(), b.Bytes(), out)
}

// do makes a request, again after a 429's wait if it ends before ctx does,
// at most three times in all. An error never quotes the URL, which can hold
// an interaction's token.
func (c *Client) do(ctx context.Context, method, path, ctype string, body []byte, out any) error {
	var err error
	for range 3 {
		var wait time.Duration
		if wait, err = c.attempt(ctx, method, path, ctype, body, out); wait == 0 {
			return err
		}
		if d, ok := ctx.Deadline(); ok && time.Until(d) < wait {
			return err
		}
		if !sleep(ctx, wait) {
			return err
		}
	}
	return err
}

// attempt makes one request, and returns the wait a 429 names.
func (c *Client) attempt(ctx context.Context, method, path, ctype string, body []byte, out any) (time.Duration, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, r)
	if err != nil {
		return 0, errors.New("discord: bad request")
	}
	req.Header.Set("Authorization", "Bot "+c.token)
	req.Header.Set("User-Agent", "DiscordBot (https://github.com/robdevops/signalbot, 1)")
	if body != nil {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, c.scrub(fmt.Errorf("discord: %s: %w", method, err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return 0, c.scrub(fmt.Errorf("discord: %s: %w", method, err))
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		var rl struct {
			RetryAfter float64 `json:"retry_after"`
		}
		json.Unmarshal(data, &rl)
		return max(time.Duration(rl.RetryAfter*float64(time.Second)), 10*time.Millisecond), &apiError{Status: 429, Message: "rate limited"}
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		}
		json.Unmarshal(data, &e)
		return 0, &apiError{Status: resp.StatusCode, Code: e.Code, Message: e.Message}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return 0, fmt.Errorf("discord: %s: malformed answer", method)
		}
	}
	return 0, nil
}

// selfID is the bot's user ID, or "" until the gateway says.
func (c *Client) selfID() string { s, _ := c.self.Load().(string); return s }

func (c *Client) appID() string { s, _ := c.app.Load().(string); return s }

func (c *Client) usernameOf() string { s, _ := c.username.Load().(string); return s }

// WaitID returns the bot's user ID, waiting for the gateway's READY, or ""
// if ctx ends first.
func (c *Client) WaitID(ctx context.Context) string {
	select {
	case <-c.known:
	case <-ctx.Done():
	}
	return c.selfID()
}

// Name is the network's name.
func (c *Client) Name() string { return "discord" }

// BotName is the bot's display name, or "discordbot" until known.
func (c *Client) BotName() string {
	if n, _ := c.name.Load().(string); n != "" {
		return n
	}
	return "discordbot"
}

// Prompt is Discord's line in the system prompt.
func (c *Client) Prompt() string {
	return "This is a Discord chat. A message holds at most 2000 characters, so keep replies short and put the answer first; Discord shows Markdown."
}

// Caps is what Discord shows: buttons, silent messages, and an edit that
// replaces a picture in place.
func (c *Client) Caps() feature.Caps {
	return feature.Caps{Buttons: true, Silent: true, EditMedia: true, Caption: maxChars}
}

// Watch says whether the core has an event hook, so reactions are handed
// on only with one, and the commands features registered, which become
// Discord's slash commands once the bot is connected.
func (c *Client) Watch(w bot.Watch) {
	c.watch.Store(w.Events)
	c.commands.Store(&w.Commands)
}

// Owns reports whether chat is a Discord chat, by its ID's prefix, so a bot
// also on another network sends it here.
func (c *Client) Owns(chat feature.Chat) bool {
	if chat.GroupID != "" {
		return strings.HasPrefix(chat.GroupID, prefix)
	}
	return strings.HasPrefix(chat.User, prefix)
}

// sleep waits d, and reports false if ctx ended first.
func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// snowflake is a Discord ID as a number, or 0 if it isn't one.
func snowflake(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func id(n int64) string { return strconv.FormatInt(n, 10) }

// discordEpoch is when Discord's IDs start counting, in Unix milliseconds.
const discordEpoch = 1420070400000

// at is the ID Discord would give a message sent at t, on the scale of
// message IDs, for an edit's or a delete's time.
func at(t time.Time) int64 { return (t.UnixMilli() - discordEpoch) << 22 }

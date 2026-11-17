// Package telegram is a chat network for the bot: a native client for
// Telegram's Bot API that long-polls getUpdates, turns what it receives into
// messages for the core, and sends replies with Telegram's entities. Only
// the main package uses it.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/kit/httpx"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

// pollWait is how long one getUpdates waits for something to arrive.
const pollWait = 50 * time.Second

// Client is a Telegram Bot API client, by long polling.
type Client struct {
	base    string // the API's base URL, without the token
	token   string // never logged: scrub every error
	allowed map[string]bool
	http    *http.Client
	backoff time.Duration // the first wait after a failed poll; a test's shorter
	typing  time.Duration // how often Typing is renewed; Telegram's lasts 5s

	self     atomic.Value // string: the bot's user ID
	username atomic.Value // string: its @username, without the @
	name     atomic.Value // string: its first name
	known    chan struct{}
	once     sync.Once
	watch    atomic.Bool // the core has an event hook

	mu     sync.Mutex
	people map[string]string            // user ID to name, as last seen
	chats  map[string]map[string]string // group ID to the IDs and names seen there
	groups map[string]string            // group ID to title, for the groups the bot is in
	timers map[string]time.Duration     // chat ID to its auto-delete timer
	moved  map[string]string            // a group's old chat ID to its new one, after it became a supergroup
	sent   map[string]bool              // "chat/message" for messages the bot sent lately
	sentQ  []string                     // sent's keys, oldest first

	test *config.Test // nil in production
}

// New makes the client its settings say: TELEGRAM_BOT_TOKEN, the bot's
// token from BotFather; TELEGRAM_DM_USER_IDS, the user IDs that may DM it;
// and TELEGRAM_API_URL, a local Bot API server, by default Telegram's.
func New(test *config.Test) *Client {
	allowed := map[string]bool{}
	for _, id := range strings.FieldsFunc(config.Env("TELEGRAM_DM_USER_IDS", ""), func(r rune) bool {
		return r == ',' || r == ';' || unicode.IsSpace(r)
	}) {
		allowed[id] = true
	}
	if len(allowed) == 0 {
		logx.Warnf("DMs are off, and no one can add the bot to a group: TELEGRAM_DM_USER_IDS is empty; it answers only in groups it is already in")
	} else {
		logx.Noticef("DMs and adding the bot to groups: allowed from %d IDs (TELEGRAM_DM_USER_IDS)", len(allowed))
	}
	return newClient(config.Env("TELEGRAM_API_URL", "https://api.telegram.org"), config.Env("TELEGRAM_BOT_TOKEN", ""), allowed, test)
}

func newClient(base, token string, allowed map[string]bool, test *config.Test) *Client {
	return &Client{base: strings.TrimRight(base, "/"), token: token, allowed: allowed, http: httpx.Client(8),
		backoff: time.Second, typing: 4 * time.Second, known: make(chan struct{}),
		people: map[string]string{}, chats: map[string]map[string]string{}, groups: map[string]string{},
		timers: map[string]time.Duration{}, moved: map[string]string{}, sent: map[string]bool{}, test: test}
}

// scrub is err with the token taken out of its text: Go's URL errors
// quote the whole URL, which holds it.
func (c *Client) scrub(err error) error {
	if err == nil || c.token == "" || !strings.Contains(err.Error(), c.token) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), c.token, "<token>"))
}

// apiError is a refusal from the Bot API.
type apiError struct {
	Code       int
	Desc       string
	RetryAfter int   // seconds, for a 429
	MigrateTo  int64 // the chat's new ID, when a group became a supergroup
}

func (e *apiError) Error() string { return fmt.Sprintf("telegram: %d %s", e.Code, e.Desc) }

// call posts params as JSON to method and decodes the result into out.
func (c *Client) call(ctx context.Context, method string, params, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.post(ctx, method, "application/json", bytes.NewReader(body), out)
}

// upload posts fields and files, each as name to file name and bytes, as a
// multipart form to method.
func (c *Client) upload(ctx context.Context, method string, fields map[string]string, files map[string][]byte, out any) error {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	for k, v := range fields {
		w.WriteField(k, v)
	}
	for name, data := range files {
		fw, err := w.CreateFormFile(name, name+".png")
		if err != nil {
			return err
		}
		fw.Write(data)
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.post(ctx, method, w.FormDataContentType(), &b, out)
}

func (c *Client) post(ctx context.Context, method, ctype string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/bot"+c.token+"/"+method, body)
	if err != nil {
		return c.scrub(err)
	}
	req.Header.Set("Content-Type", ctype)
	resp, err := c.http.Do(req)
	if err != nil {
		return c.scrub(err)
	}
	defer resp.Body.Close()
	var r struct {
		OK          bool            `json:"ok"`
		Result      json.RawMessage `json:"result"`
		ErrorCode   int             `json:"error_code"`
		Description string          `json:"description"`
		Parameters  struct {
			RetryAfter int   `json:"retry_after"`
			MigrateTo  int64 `json:"migrate_to_chat_id"`
		} `json:"parameters"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&r); err != nil {
		return fmt.Errorf("telegram: %s: bad response (status %d)", method, resp.StatusCode)
	}
	if !r.OK {
		return &apiError{Code: r.ErrorCode, Desc: r.Description, RetryAfter: r.Parameters.RetryAfter, MigrateTo: r.Parameters.MigrateTo}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(r.Result, out); err != nil {
		return fmt.Errorf("telegram: %s: malformed result", method)
	}
	return nil
}

// self is the bot's user ID, or "" until getMe answers.
func (c *Client) selfID() string { s, _ := c.self.Load().(string); return s }

// WaitID returns the bot's user ID, waiting for getMe, or "" if ctx ends
// first.
func (c *Client) WaitID(ctx context.Context) string {
	select {
	case <-c.known:
	case <-ctx.Done():
	}
	return c.selfID()
}

// BotName is the bot's first name, or "telegrambot" until known.
func (c *Client) BotName() string {
	if n, _ := c.name.Load().(string); n != "" {
		return n
	}
	return "telegrambot"
}

// Prompt is Telegram's line in the system prompt.
func (c *Client) Prompt() string {
	return "This is a Telegram chat. A message holds at most 4096 characters, so keep replies short and put the answer first."
}

// Watch says whether the core has an event hook, so updates only events
// would use are skipped without one.
func (c *Client) Watch(events bool) { c.watch.Store(events) }

// Run learns who the bot is, then long-polls getUpdates and hands each
// update the core may act on to handle, on this goroutine, until ctx ends.
// A failed poll is retried with backoff, a 429 after the wait it names.
func (c *Client) Run(ctx context.Context, handle func(bot.Received)) {
	backoff := c.backoff
	for ctx.Err() == nil && c.selfID() == "" {
		if err := c.hello(ctx); err != nil {
			logx.Warnf("telegram getMe failed: %v; retrying in %s", err, backoff)
			if !sleep(ctx, backoff) {
				return
			}
			backoff = min(2*backoff, 32*time.Second)
		}
	}
	backoff = c.backoff
	var offset, asked int64 // the next update wanted, and the offset last sent, which confirmed those before it
	defer func() {
		if offset > asked { // handled but unconfirmed: confirm them, or a restart gets them again
			cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := c.call(cctx, "getUpdates", map[string]any{"offset": offset, "timeout": 0, "limit": 1}, nil); err != nil {
				logx.Warnf("telegram: confirming the last updates failed: %v", err)
			}
		}
	}()
	for ctx.Err() == nil {
		asked = offset
		ups, err := c.poll(ctx, offset)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			wait := backoff
			var ae *apiError
			if errors.As(err, &ae) && ae.RetryAfter > 0 {
				wait = time.Duration(ae.RetryAfter) * time.Second
			} else {
				backoff = min(2*backoff, 32*time.Second)
			}
			logx.Warnf("telegram getUpdates failed: %v; retrying in %s", err, wait)
			if !sleep(ctx, wait) {
				return
			}
			continue
		}
		backoff = c.backoff
		for _, u := range ups {
			offset = max(offset, u.UpdateID+1)
			func() {
				defer logx.Recover("reading an update")
				if r, ok := c.event(u); ok {
					handle(r)
				}
			}()
			if c.test != nil && c.test.Handled != nil {
				c.test.Handled()
			}
		}
	}
}

// hello asks getMe who the bot is, and drops any webhook, which would make
// getUpdates fail.
func (c *Client) hello(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var me struct {
		ID        int64  `json:"id"`
		FirstName string `json:"first_name"`
		Username  string `json:"username"`
	}
	if err := c.call(ctx, "getMe", struct{}{}, &me); err != nil {
		return err
	}
	if err := c.call(ctx, "deleteWebhook", map[string]bool{"drop_pending_updates": false}, nil); err != nil {
		return err
	}
	c.name.Store(me.FirstName)
	c.username.Store(me.Username)
	c.self.Store(strconv.FormatInt(me.ID, 10))
	c.once.Do(func() { close(c.known) })
	logx.Noticef("connected to Telegram as @%s", me.Username)
	return nil
}

// poll is one getUpdates.
func (c *Client) poll(ctx context.Context, offset int64) ([]update, error) {
	ctx, cancel := context.WithTimeout(ctx, pollWait+20*time.Second)
	defer cancel()
	kinds := []string{"message", "edited_message", "my_chat_member"}
	if c.watch.Load() {
		kinds = append(kinds, "message_reaction", "chat_member")
	}
	var ups []update
	err := c.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": int(pollWait.Seconds()), "allowed_updates": kinds}, &ups)
	return ups, err
}

// retrying runs a send, again after a 429's wait if it ends before ctx
// does, and again at a group's new ID if it became a supergroup, at most
// three times in all. move is given the new chat ID.
func (c *Client) retrying(ctx context.Context, from string, move func(chatID string), send func() error) error {
	var err error
	for range 3 {
		var ae *apiError
		if err = send(); !errors.As(err, &ae) {
			return err
		}
		switch {
		case ae.MigrateTo != 0:
			to := strconv.FormatInt(ae.MigrateTo, 10)
			c.mu.Lock()
			c.moved[from] = to
			c.mu.Unlock()
			move(to)
		case ae.RetryAfter > 0:
			wait := time.Duration(ae.RetryAfter) * time.Second
			if d, ok := ctx.Deadline(); ok && time.Until(d) < wait {
				return err
			}
			if !sleep(ctx, wait) {
				return err
			}
		default:
			return err
		}
	}
	return err
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

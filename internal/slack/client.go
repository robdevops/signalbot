// Package slack is a chat network for the bot: a native client for Slack's
// Web API and Socket Mode, which pushes events over a WebSocket the bot
// opens, so it needs no public address. It turns what it receives into
// messages for the core, and sends replies as Slack's blocks. Only the main
// package uses it.
package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/httpx"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/state"
	"github.com/robdevops/signalbot/internal/kit/txt"
	"github.com/robdevops/signalbot/internal/kit/ws"
)

// prefix starts every chat and user ID the client hands the core, so a bot
// also on another network knows Slack's chats by their shape.
const prefix = "slack:"

// Client is a Slack client, by Socket Mode.
type Client struct {
	base     string // the Web API's base URL, such as https://slack.com/api
	token    string // the bot token, xoxb-…; never logged: scrub every error
	appToken string // the app-level token, xapp-…, for Socket Mode; never logged
	stateDir string // where the app manifest is written
	http     *http.Client
	backoff  time.Duration // the first wait after a failed connection; a test's shorter
	slow     time.Duration // the wait after Slack refuses a token
	settle   time.Duration // the wait for Slack to process an uploaded picture

	self   atomic.Value // string: the bot's user ID, without the prefix
	handle atomic.Value // string: its @name
	name   atomic.Value // string: its display name
	named  atomic.Bool  // users.info gave the display name, not auth.test's @name
	naming atomic.Bool  // a users.info call for it is under way
	known  chan struct{}
	once   sync.Once
	hellos atomic.Int64 // Socket Mode connections made

	watch    atomic.Bool                   // the core has an event hook
	commands atomic.Pointer[[]bot.Command] // the commands features registered, for the app manifest

	mu      sync.Mutex
	people  map[string]string            // user ID to display name, as last seen
	asking  map[string]bool              // user IDs whose names are being fetched
	ims     map[string]string            // user ID to the DM channel with the bot
	imUser  map[string]string            // a DM channel to its user
	chans   map[string]map[string]string // channel ID to the user IDs seen there, each to the day last seen, 2006-01-02
	left    map[string]bool              // channels the bot was removed from, so it no longer posts there
	sent    map[string]bool              // "channel/ts" for messages the bot sent lately
	sentQ   []string                     // sent's keys, oldest first
	thread  map[string]string            // "channel/ts" to its thread's ts, for messages in threads the bot sent or read lately
	threadQ []string                     // thread's keys, oldest first
	blocks  map[string][]any             // "channel/ts" to the blocks the bot last put in it, but buttons
	blockQ  []string                     // blocks' keys, oldest first

	saved     *state.Doc    // slack-people.json: the people seen in each channel, with their names
	dirty     atomic.Bool   // people or chans changed since they were saved
	saveEvery time.Duration // how often they are saved while changed; a test's shorter
	scopeWarn sync.Once     // Groups' missing scopes are logged once

	test *config.Test // nil in production
}

// Check reports a setting the Slack client can't start without:
// SLACK_APP_TOKEN, with SLACK_BOT_TOKEN set.
func Check() error {
	bt, app := config.Env("SLACK_BOT_TOKEN", ""), config.Env("SLACK_APP_TOKEN", "")
	switch {
	case !strings.HasPrefix(bt, "xoxb-"):
		return errors.New("SLACK_BOT_TOKEN is not a bot token: copy the Bot User OAuth Token, xoxb-…, from api.slack.com/apps → your app → OAuth & Permissions")
	case app == "":
		return errors.New("SLACK_APP_TOKEN is not set: the bot reaches Slack by Socket Mode, which needs an app-level token, xapp-…, with connections:write, from api.slack.com/apps → your app → Basic Information → App-Level Tokens")
	case !strings.HasPrefix(app, "xapp-"):
		return errors.New("SLACK_APP_TOKEN is not an app-level token: it starts xapp-; make one with connections:write at api.slack.com/apps → your app → Basic Information → App-Level Tokens")
	}
	return nil
}

// New makes the client its settings say: SLACK_BOT_TOKEN, the bot's token;
// SLACK_APP_TOKEN, the app-level token Socket Mode needs; and SLACK_API_URL,
// by default Slack's. The bot answers anyone who DMs it and any channel it
// is in: the workspace's settings decide who can reach it.
func New(test *config.Test) *Client {
	return newClient(config.Env("SLACK_API_URL", "https://slack.com/api"), config.Env("SLACK_BOT_TOKEN", ""), config.Env("SLACK_APP_TOKEN", ""), config.StateDir(), test)
}

func newClient(base, token, appToken, stateDir string, test *config.Test) *Client {
	c := &Client{base: strings.TrimRight(base, "/"), token: token, appToken: appToken, stateDir: stateDir, http: httpx.Client(8),
		backoff: time.Second, slow: 10 * time.Minute, settle: 2 * time.Second, known: make(chan struct{}),
		people: map[string]string{}, asking: map[string]bool{}, ims: map[string]string{}, imUser: map[string]string{},
		chans: map[string]map[string]string{}, left: map[string]bool{}, sent: map[string]bool{}, thread: map[string]string{}, blocks: map[string][]any{},
		saved: state.Open(state.Dir(stateDir), peopleName), saveEvery: time.Minute, test: test}
	c.loadPeople()
	return c
}

// scrub is err with the tokens taken out of its text.
func (c *Client) scrub(err error) error {
	if err == nil {
		return nil
	}
	s := err.Error()
	for _, t := range []string{c.token, c.appToken} {
		if t != "" {
			s = strings.ReplaceAll(s, t, "<token>")
		}
	}
	if s == err.Error() {
		return err
	}
	return errors.New(s)
}

// apiError is a refusal from the Web API.
type apiError struct {
	Method     string
	Code       string // Slack's error, such as "invalid_auth"
	RetryAfter int    // seconds, for a 429
}

func (e *apiError) Error() string { return fmt.Sprintf("slack: %s: %s", e.Method, e.Code) }

// code is err's Slack error, or "".
func code(err error) string {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

// refusedToken reports whether err is Slack refusing a token for good.
func refusedToken(err error) bool {
	switch code(err) {
	case "invalid_auth", "not_authed", "account_inactive", "token_revoked", "token_expired", "not_allowed_token_type":
		return true
	}
	return false
}

// call posts params as JSON to method with the bot token, and decodes the
// response into out.
func (c *Client) call(ctx context.Context, method string, params, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	return c.post(ctx, method, c.token, "application/json; charset=utf-8", body, out)
}

// form posts vals as a form to method, for the methods that take no JSON,
// with token.
func (c *Client) form(ctx context.Context, method, token string, vals url.Values, out any) error {
	return c.post(ctx, method, token, "application/x-www-form-urlencoded", []byte(vals.Encode()), out)
}

// post makes one Web API call, again after a 429's wait if it ends before
// ctx does, at most three times in all.
func (c *Client) post(ctx context.Context, method, token, ctype string, body []byte, out any) error {
	var err error
	for range 3 {
		var ae *apiError
		if err = c.postOnce(ctx, method, token, ctype, body, out); !errors.As(err, &ae) || ae.RetryAfter == 0 {
			return err
		}
		wait := time.Duration(ae.RetryAfter) * time.Second
		if d, ok := ctx.Deadline(); ok && time.Until(d) < wait || !sleep(ctx, wait) {
			return err
		}
	}
	return err
}

func (c *Client) postOnce(ctx context.Context, method, token, ctype string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/"+method, bytes.NewReader(body))
	if err != nil {
		return c.scrub(err)
	}
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.http.Do(req)
	if err != nil {
		return c.scrub(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		n, _ := strconv.Atoi(resp.Header.Get("Retry-After"))
		return &apiError{Method: method, Code: "ratelimited", RetryAfter: max(n, 1)}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return c.scrub(err)
	}
	var r struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return fmt.Errorf("slack: %s: bad response (status %d)", method, resp.StatusCode)
	}
	if !r.OK {
		return &apiError{Method: method, Code: txt.Clamp(r.Error, 80)}
	}
	if out != nil && json.Unmarshal(data, out) != nil {
		return fmt.Errorf("slack: %s: malformed result", method)
	}
	return nil
}

// selfID is the bot's user ID, without the prefix, or "" until auth.test
// answers.
func (c *Client) selfID() string { s, _ := c.self.Load().(string); return s }

// WaitID returns the bot's user ID, waiting for auth.test, or "" if ctx
// ends first.
func (c *Client) WaitID(ctx context.Context) string {
	select {
	case <-c.known:
	case <-ctx.Done():
	}
	if s := c.selfID(); s != "" {
		return prefix + s
	}
	return ""
}

// Name is the network's name.
func (c *Client) Name() string { return "slack" }

// BotName is the bot's display name, or "signalbot" until known.
func (c *Client) BotName() string {
	if n, _ := c.name.Load().(string); n != "" {
		return n
	}
	return "signalbot"
}

func (c *Client) handleOf() string { h, _ := c.handle.Load().(string); return h }

// Prompt is Slack's line in the system prompt.
func (c *Client) Prompt() string {
	return "This is a Slack chat. Keep replies short and put the answer first; Slack shows bold, italic, code and links, but no headings or tables."
}

// Caps is what Slack shows: buttons, and pictures an edit can replace.
func (c *Client) Caps() feature.Caps {
	return feature.Caps{Buttons: true, EditMedia: true, Caption: maxSection}
}

// Owns reports whether chat is a Slack chat, by its prefix.
func (c *Client) Owns(chat feature.Chat) bool {
	if chat.GroupID != "" {
		return strings.HasPrefix(chat.GroupID, prefix)
	}
	return strings.HasPrefix(chat.User, prefix)
}

// Watch says whether the core has an event hook, so reactions and member
// changes are skipped without one, and which commands features registered,
// for the app manifest's slash commands, written once the bot knows its
// name.
func (c *Client) Watch(w bot.Watch) {
	c.watch.Store(w.Events)
	c.commands.Store(&w.Commands)
}

// Run learns who the bot is, then holds a Socket Mode connection open and
// hands each event the core may act on to handle, on this goroutine, until
// ctx ends, saving the people seen at most once a minute while they
// change, and once more as it returns. A dropped connection is opened
// again with backoff, from 1s to 32s; one Slack asks to close is opened
// again at once. A token Slack refuses is logged naming its setting, and
// tried again only every 10 minutes.
func (c *Client) Run(ctx context.Context, handle func(bot.Received)) {
	saver := make(chan struct{})
	go func() {
		defer close(saver)
		c.keepPeople(ctx)
	}()
	defer func() {
		<-saver
		c.savePeople()
	}()
	backoff := c.backoff
	for ctx.Err() == nil && c.selfID() == "" {
		err := c.hello(ctx)
		if err == nil {
			break
		}
		wait := backoff
		if refusedToken(err) {
			wait = c.slow
			logx.Errorf("Slack refused SLACK_BOT_TOKEN (%s): set it to the Bot User OAuth Token, xoxb-…, from api.slack.com/apps → your app → OAuth & Permissions; trying again in %s", code(err), wait)
		} else {
			backoff = min(2*backoff, 32*time.Second)
			logx.Warnf("slack auth.test failed: %v; retrying in %s", err, wait)
		}
		if !sleep(ctx, wait) {
			return
		}
	}
	c.writeManifest()
	backoff = c.backoff
	for ctx.Err() == nil {
		hello, planned, err := c.connect(ctx, handle)
		if ctx.Err() != nil {
			return
		}
		if hello {
			backoff = c.backoff
		}
		if planned {
			logx.Debugf("slack: reconnecting, as asked")
			continue
		}
		wait := backoff
		if refusedToken(err) {
			wait = c.slow
			logx.Errorf("Slack refused SLACK_APP_TOKEN (%s): set it to an app-level token, xapp-…, with connections:write, from api.slack.com/apps → your app → Basic Information, and turn on Socket Mode; trying again in %s", code(err), wait)
		} else {
			backoff = min(2*backoff, 32*time.Second)
			logx.Warnf("slack connection lost: %v; reconnecting in %s", err, wait)
		}
		if !sleep(ctx, wait) {
			return
		}
	}
}

// hello asks auth.test who the bot is, and users.info for its display
// name; if that fails, its @name stands in until a later Socket Mode hello
// asks again.
func (c *Client) hello(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var me struct {
		UserID string `json:"user_id"`
		User   string `json:"user"`
	}
	if err := c.form(ctx, "auth.test", c.token, nil, &me); err != nil {
		return err
	}
	if me.UserID == "" {
		return errors.New("slack: auth.test: no user ID: is SLACK_BOT_TOKEN a bot token?")
	}
	name := me.User
	u, err := c.userInfo(ctx, me.UserID)
	if err != nil {
		logx.Debugf("slack users.info for the bot failed: %v", err)
	} else if u != "" {
		name = u
	}
	c.handle.Store(me.User)
	c.name.Store(name)
	c.named.Store(err == nil)
	c.self.Store(me.UserID)
	c.once.Do(func() { close(c.known) })
	return nil
}

// rename asks users.info again for the bot's display name, after it failed
// at the start, and writes the app manifest with it. One runs at a time.
func (c *Client) rename(ctx context.Context) {
	if c.named.Load() || !c.naming.CompareAndSwap(false, true) {
		return
	}
	defer c.naming.Store(false)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	u, err := c.userInfo(ctx, c.selfID())
	if err != nil {
		logx.Debugf("slack users.info for the bot failed: %v", err)
		return
	}
	if u != "" {
		c.name.Store(u)
	}
	c.named.Store(true)
	c.writeManifest()
}

// envelope is one Socket Mode message.
type envelope struct {
	Type       string          `json:"type"`
	EnvelopeID string          `json:"envelope_id"`
	Payload    json.RawMessage `json:"payload"`
	Reason     string          `json:"reason"` // a disconnect's
}

// connect opens a Socket Mode connection and reads it until it fails or
// Slack asks to close it (planned), acknowledging every envelope at once,
// before it is handled, as Slack wants within 3 seconds. hello says Slack
// greeted the connection.
func (c *Client) connect(ctx context.Context, handle func(bot.Received)) (hello, planned bool, err error) {
	octx, cancel := context.WithTimeout(ctx, 30*time.Second)
	var open struct {
		URL string `json:"url"`
	}
	err = c.form(octx, "apps.connections.open", c.appToken, nil, &open)
	if err == nil {
		var conn *ws.Conn
		if conn, err = ws.Dial(octx, c.http, open.URL, nil, 4<<20); err == nil {
			cancel()
			defer conn.Close(1000, "")
			return c.read(ctx, conn, handle)
		}
		err = c.scrub(dialError(err))
	}
	cancel()
	return false, false, err
}

// dialError is a failed dial's err without the URL, which holds a ticket
// for the connection.
func dialError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("slack: socket: %s: %w", ue.Op, ue.Err)
	}
	return err
}

// read reads conn until it fails or Slack asks to close it.
func (c *Client) read(ctx context.Context, conn *ws.Conn, handle func(bot.Received)) (hello, planned bool, err error) {
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return hello, false, c.scrub(err)
		}
		var env envelope
		if json.Unmarshal(data, &env) != nil {
			continue
		}
		if env.EnvelopeID != "" {
			ack, _ := json.Marshal(map[string]string{"envelope_id": env.EnvelopeID})
			if err := conn.Write(ctx, true, ack); err != nil {
				return hello, false, c.scrub(err)
			}
		}
		switch env.Type {
		case "hello":
			hello = true
			if c.hellos.Add(1) == 1 {
				logx.Noticef("connected to Slack as @%s", c.handleOf())
			} else {
				logx.Debugf("slack: connected again")
			}
			if !c.named.Load() {
				go c.rename(ctx)
			}
		case "disconnect":
			return hello, true, nil
		default:
			func() {
				defer logx.Recover("reading an event")
				for _, r := range c.events(env) {
					handle(r)
				}
			}()
		}
		if c.test != nil && c.test.Handled != nil {
			c.test.Handled()
		}
	}
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

package discord

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/kit/txt"
	"github.com/robdevops/signalbot/internal/kit/ws"
)

// The gateway's opcodes the bot uses.
const (
	opDispatch  = 0
	opHeartbeat = 1
	opIdentify  = 2
	opResume    = 6
	opReconnect = 7
	opInvalid   = 9
	opHello     = 10
	opAck       = 11
)

// intents are the events the bot asks the gateway for: guilds, their
// members, their messages and reactions, DMs and their reactions, and
// messages' content. Messages' content is privileged and must be turned
// on in the developer portal; so must members, but the bot does without.
const intents = 1<<0 | guildMembers | 1<<9 | 1<<10 | 1<<12 | 1<<13 | 1<<15

// readLimit caps one gateway message.
const readLimit = 4 << 20

// payload is one gateway message.
type payload struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  int64           `json:"s"`
	T  string          `json:"t"`
}

var (
	errReconnect = errors.New("asked to reconnect")
	errInvalid   = errors.New("the session ended")
	errMembers   = errors.New("Discord refused the Server Members intent")
)

// refusal is Discord refusing the bot's settings: retrying soon won't help.
type refusal struct{ msg string }

func (r *refusal) Error() string { return r.msg }

var errToken = &refusal{"refused DISCORD_BOT_TOKEN: copy the token again from the developer portal (your application, Bot, Reset Token) and set it"}

// Run connects to the gateway and hands each event the core may act on to
// handle, on this goroutine, until ctx ends. A dropped session is resumed,
// after a wait that doubles from 1s to 32s while it keeps failing; a token
// or intents Discord refuses are logged as such, and tried again only
// after a long wait; but if it refuses the Server Members intent, the bot
// identifies again at once without it, and never asks for it again.
func (c *Client) Run(ctx context.Context, handle func(bot.Received)) {
	backoff := c.backoff
	for ctx.Err() == nil {
		up, err := c.connect(ctx, handle)
		if ctx.Err() != nil {
			return
		}
		if up {
			backoff = c.backoff
		}
		var r *refusal
		wait := backoff
		switch {
		case errors.Is(err, errMembers):
			if c.membersOff.CompareAndSwap(false, true) {
				log.Warnf(membersWarning)
			}
			c.session, c.resumeURL = "", ""
			c.seq.Store(0)
			continue
		case errors.As(err, &r):
			log.Errorf("%s; trying again in %s", r.msg, c.fatal)
			wait = c.fatal
			if !sleep(ctx, wait) {
				return
			}
			continue
		case errors.Is(err, errInvalid): // a new session, after 1 to 5 seconds
			wait = c.backoff + rand.N(4*c.backoff)
		default:
			backoff = min(2*backoff, 32*time.Second)
		}
		log.Warnf("connection lost: %v; reconnecting in %s", err, wait.Round(time.Millisecond))
		if !sleep(ctx, wait) {
			return
		}
	}
}

// connect runs one gateway connection until it drops: it resumes the last
// session if there is one, or else identifies afresh. up reports whether
// the session was established, by READY or RESUMED.
func (c *Client) connect(ctx context.Context, handle func(bot.Received)) (up bool, err error) {
	if err := c.checkApp(ctx); err != nil {
		return false, err
	}
	url, resume := c.resumeURL, c.session != "" && c.resumeURL != ""
	if !resume {
		var g struct {
			URL string `json:"url"`
		}
		if err := c.call(ctx, http.MethodGet, "/gateway/bot", nil, &g); err != nil {
			if status(err, http.StatusUnauthorized) {
				return false, errToken
			}
			return false, fmt.Errorf("asking for the gateway: %w", err)
		}
		url = g.URL
	}
	conn, err := ws.Dial(ctx, c.http, url+"?v=10&encoding=json", nil, readLimit)
	if err != nil {
		return false, c.scrub(err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer conn.Close(4000, "") // not 1000, which would end the session
	_, data, err := conn.Read(ctx)
	var hello struct {
		Op int `json:"op"`
		D  struct {
			Interval int64 `json:"heartbeat_interval"`
		} `json:"d"`
	}
	if err != nil {
		return false, c.closed(err, false)
	}
	if json.Unmarshal(data, &hello) != nil || hello.Op != opHello || hello.D.Interval <= 0 {
		return false, errors.New("no hello from the gateway")
	}
	var acked atomic.Bool
	acked.Store(true)
	go c.heartbeat(ctx, conn, time.Duration(hello.D.Interval)*time.Millisecond, &acked)
	members := !c.membersOff.Load()
	if resume {
		err = c.write(ctx, conn, opResume, map[string]any{"token": c.token, "session_id": c.session, "seq": c.seq.Load()})
	} else {
		in := intents
		if !members {
			in &^= guildMembers
		}
		err = c.write(ctx, conn, opIdentify, map[string]any{"token": c.token, "intents": in,
			"properties": map[string]string{"os": "linux", "browser": "signalbot", "device": "signalbot"}})
	}
	if err != nil {
		return false, c.scrub(err)
	}
	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return up, c.closed(err, members)
		}
		var p payload
		if json.Unmarshal(data, &p) != nil {
			continue
		}
		if p.S != 0 {
			c.seq.Store(p.S)
		}
		switch p.Op {
		case opDispatch:
			switch p.T {
			case "READY":
				up = c.ready(p.D)
			case "RESUMED":
				up = true
				log.Noticef("resumed the session as %s", c.usernameOf())
			}
			func() {
				defer recoverEvent()
				for _, r := range c.events(p.T, p.D) {
					handle(r)
				}
			}()
		case opHeartbeat:
			c.beat(ctx, conn)
		case opReconnect:
			return up, errReconnect
		case opInvalid:
			var resumable bool
			json.Unmarshal(p.D, &resumable)
			if !resumable {
				c.session, c.resumeURL = "", ""
				c.seq.Store(0)
			}
			return up, errInvalid
		case opAck:
			acked.Store(true)
		}
		if c.test != nil && c.test.Handled != nil {
			c.test.Handled()
		}
	}
}

// closed is the error a dropped connection stands for: Discord's close
// codes for a refused token or intents are refusals, but refused intents
// that had the Server Members intent, which may be what it refused; and
// those for a session it won't resume start a new one.
func (c *Client) closed(err error, members bool) error {
	var ce *ws.CloseError
	if !errors.As(err, &ce) {
		return c.scrub(err)
	}
	switch ce.Code {
	case 4004:
		return errToken
	case 4014:
		if members {
			return errMembers
		}
		return &refusal{"refused the bot's intents (close 4014): in the developer portal, under your application's Bot, turn on Message Content Intent"}
	case 4010, 4011, 4012, 4013:
		return &refusal{fmt.Sprintf("refused the bot's connection (close %d)", ce.Code)}
	case 4003, 4007, 4009:
		c.session, c.resumeURL = "", ""
		c.seq.Store(0)
	}
	return fmt.Errorf("closed by Discord (%d)", ce.Code)
}

// write sends one gateway message.
func (c *Client) write(ctx context.Context, conn *ws.Conn, op int, d any) error {
	data, err := json.Marshal(map[string]any{"op": op, "d": d})
	if err != nil {
		return err
	}
	return conn.Write(ctx, true, data)
}

// beat sends a heartbeat with the last sequence number.
func (c *Client) beat(ctx context.Context, conn *ws.Conn) {
	var s any
	if n := c.seq.Load(); n != 0 {
		s = n
	}
	if err := c.write(ctx, conn, opHeartbeat, s); err != nil && ctx.Err() == nil {
		log.Debugf("heartbeat failed: %v", err)
	}
}

// heartbeat beats every interval, the first after a random part of it, as
// Discord asks, until ctx ends; a beat whose ack hasn't come by the next
// closes the connection, so it is resumed.
func (c *Client) heartbeat(ctx context.Context, conn *ws.Conn, every time.Duration, acked *atomic.Bool) {
	t := time.NewTimer(rand.N(every))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !acked.Swap(false) {
			log.Warnf("no heartbeat ack, so reconnecting")
			conn.Close(4000, "")
			return
		}
		c.beat(ctx, conn)
		t.Reset(every)
	}
}

// ready is the gateway's READY: who the bot is, the session to resume, and
// the guilds it is in, so that a GUILD_CREATE for another is the bot being
// added. It reports whether READY was read.
func (c *Client) ready(d json.RawMessage) bool {
	var r struct {
		SessionID string `json:"session_id"`
		ResumeURL string `json:"resume_gateway_url"`
		User      user   `json:"user"`
		App       struct {
			ID string `json:"id"`
		} `json:"application"`
		Guilds []struct {
			ID string `json:"id"`
		} `json:"guilds"`
	}
	if json.Unmarshal(d, &r) != nil || r.User.ID == "" {
		return false
	}
	ids := make([]string, 0, len(r.Guilds))
	for _, g := range r.Guilds {
		if g.ID != "" && !slices.Contains(ids, g.ID) {
			ids = append(ids, g.ID)
		}
	}
	c.joinedFrom(ids)
	c.session, c.resumeURL = r.SessionID, r.ResumeURL
	c.username.Store(r.User.Username)
	c.name.Store(cmp.Or(txt.SafeName(r.User.GlobalName), r.User.Username))
	if r.App.ID != "" {
		c.app.Store(r.App.ID)
	}
	c.self.Store(r.User.ID)
	c.once.Do(func() { close(c.known) })
	log.Noticef("connected as %s", r.User.Username)
	if !c.listed.Swap(true) {
		go c.publishCommands()
	}
	return true
}

// checkApp asks, once, for the bot's application: its ID, and whether
// anyone may add the bot to a server, or install it on their own account
// and use its commands anywhere, each warned about.
func (c *Client) checkApp(ctx context.Context) error {
	if c.checked.Load() {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var app struct {
		ID     string                     `json:"id"`
		Public bool                       `json:"bot_public"`
		Types  map[string]json.RawMessage `json:"integration_types_config"` // "1" is user install
	}
	err := c.call(ctx, http.MethodGet, "/applications/@me", nil, &app)
	switch {
	case status(err, http.StatusUnauthorized):
		return errToken
	case err != nil:
		log.Warnf("reading the application failed: %v", err)
		return nil // asked again on the next connection
	}
	c.checked.Store(true)
	if app.ID != "" {
		c.app.Store(app.ID)
	}
	if app.Public {
		log.Warnf("anyone can add this bot to their server: in the developer portal, under your application's Installation and Bot, turn off Public Bot")
	}
	if _, ok := app.Types["1"]; ok {
		log.Warnf("anyone can install this bot on their own account and use its commands anywhere, without sharing a server: in the developer portal, under your application's Installation, untick User Install")
	}
	return nil
}

// commandName is a name Discord's slash commands take, as features' are:
// lowercase letters, digits, - and _, up to 32.
var commandName = regexp.MustCompile(`^[-_a-z0-9]{1,32}$`)

// publishCommands makes the commands features registered Discord's slash
// commands, each with what it is about, or its own name for a command with
// no label, and one optional text option for its arguments; a name Discord
// doesn't take is left out, as are any past its 100. A failure is only
// logged: the commands still work when typed after the bot's @mention.
func (c *Client) publishCommands() {
	cmds, app := c.commands.Load(), c.appID()
	if cmds == nil || app == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	list := []map[string]any{}
	for _, cmd := range *cmds {
		if commandName.MatchString(cmd.Name) && len(list) < 100 {
			list = append(list, map[string]any{"name": cmd.Name, "description": txt.Clamp(cmp.Or(cmd.About, cmd.Name), 99), "type": 1,
				"options": []map[string]any{{"type": 3, "name": "text", "description": "what to add", "required": false}}})
		}
	}
	if err := c.call(ctx, http.MethodPut, "/applications/"+app+"/commands", list, nil); err != nil {
		log.Warnf("listing the slash commands failed: %v", err)
	}
}

// joinedFrom notes READY's guilds. The first READY of the process starts
// the startup line of the bot's servers, logged once all of them have
// come in GUILD_CREATE, or after c.settle with the others by ID alone. A
// later one, a new session after a dropped one, keeps only the guilds the
// bot was in and still is, so one it was added to meanwhile is warned of
// when its GUILD_CREATE comes.
func (c *Client) joinedFrom(ids []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.joined != nil {
		still := make(map[string]bool, len(ids))
		for _, id := range ids {
			if c.joined[id] {
				still[id] = true
			}
		}
		c.joined = still
		return
	}
	c.joined = make(map[string]bool, len(ids))
	for _, id := range ids {
		c.joined[id] = true
	}
	c.startup, c.arrived = ids, map[string]string{}
	if len(ids) == 0 {
		c.logServers()
		return
	}
	time.AfterFunc(c.settle, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.logServers()
	})
}

// arrivedGuild notes a guild's GUILD_CREATE for the startup line, logged
// once the last of READY's has come, or warns that the bot was added to
// it if READY didn't list it. Called with c.mu held.
func (c *Client) arrivedGuild(id, name string) {
	if c.joined == nil { // before READY
		return
	}
	if !c.joined[id] {
		c.joined[id] = true
		log.Warnf("the bot was added to a server: %s", serverLabel(id, name))
		return
	}
	if c.startup == nil || !slices.Contains(c.startup, id) {
		return
	}
	c.arrived[id] = serverLabel(id, name)
	if len(c.arrived) == len(c.startup) {
		c.logServers()
	}
}

// logServers logs, once, the servers the bot is in, by name and ID, so its
// admin sees one it was added to by someone else: READY's, in its order,
// one that hasn't come yet by its ID alone. Called with c.mu held.
func (c *Client) logServers() {
	if c.startup == nil {
		return
	}
	parts := make([]string, 0, len(c.startup))
	for _, id := range c.startup {
		parts = append(parts, cmp.Or(c.arrived[id], "(unavailable) ("+id+")"))
	}
	log.Noticef("servers (%d): %s", len(parts), cmp.Or(strings.Join(parts, ", "), "none"))
	c.startup, c.arrived = nil, nil
}

// serverLabel is a server as the log names it, "Name (id)", its name as
// txt.LogName shows one. Server names are logged only in these lines.
func serverLabel(id, name string) string {
	return txt.LogName(name) + " (" + id + ")"
}

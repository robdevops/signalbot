package discord

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// interaction is a slash command or a button press: what its answer is
// sent with. A slash command's token works for 15 minutes.
type interaction struct {
	id         int64
	app, token string
	at         time.Time
	timer      *time.Timer   // takes away its "thinking…" if no reply comes
	acked      chan struct{} // closed once it is acknowledged, or that failed
}

// tokenLife is how long an interaction's token works, less a margin.
const tokenLife = 14 * time.Minute

// interaction is a button press or a slash command as the core gets it.
// Either is acknowledged at once, as Discord wants within 3 seconds: a
// press with nothing shown, a slash command with "thinking…", which the
// reply to it replaces.
func (c *Client) interaction(d json.RawMessage) bot.Received {
	var e struct {
		ID        string  `json:"id"`
		App       string  `json:"application_id"`
		Type      int     `json:"type"`
		Token     string  `json:"token"`
		ChannelID string  `json:"channel_id"`
		GuildID   string  `json:"guild_id"`
		Member    *member `json:"member"`
		User      *user   `json:"user"`
		Message   *struct {
			ID string `json:"id"`
		} `json:"message"`
		Data struct {
			Name     string `json:"name"`
			CustomID string `json:"custom_id"`
			Options  []struct {
				Name  string `json:"name"`
				Value any    `json:"value"`
			} `json:"options"`
		} `json:"data"`
	}
	if json.Unmarshal(d, &e) != nil || e.Token == "" {
		return nil
	}
	ix := &interaction{id: snowflake(e.ID), app: e.App, token: e.Token, at: time.Now(), acked: make(chan struct{})}
	who := e.User
	if e.Member != nil && e.Member.User != nil {
		who = e.Member.User
	}
	if who == nil || who.ID == c.selfID() || who.Bot {
		go c.respond(ix, e.ID, 6)
		return nil
	}
	ch := chatOf(e.GuildID, e.ChannelID, who.ID)
	by := feature.Person{ID: prefix + who.ID, Name: nameOf(who, e.Member)}
	c.mu.Lock()
	if e.GuildID == "" {
		c.dms[who.ID], c.dmUsers[e.ChannelID] = e.ChannelID, who.ID
	} else if by.Name != "" && len(c.people) < 10000 {
		c.people[who.ID] = by.Name
	}
	c.mu.Unlock()
	switch {
	case e.Type == 3 && e.Message != nil && e.Data.CustomID != "": // a button
		go c.respond(ix, e.ID, 6)
		return bot.Pressed{Press: feature.Press{Chat: ch, By: by, Sent: snowflake(e.Message.ID), Data: e.Data.CustomID}, Answer: func(text string) {
			if text = strings.TrimSpace(text); text != "" {
				go c.followUp(ix, text)
			}
		}}
	case e.Type == 2 && e.Data.Name != "": // a slash command
		go c.respond(ix, e.ID, 5)
		text := "/" + e.Data.Name
		for _, o := range e.Data.Options {
			if s, ok := o.Value.(string); ok && o.Name == "text" && strings.TrimSpace(s) != "" {
				text += " " + strings.TrimSpace(s)
			}
		}
		c.await(ch, ix)
		msg := feature.Message{Chat: ch, Sender: by.ID, Name: by.Name, Text: text, Timestamp: ix.id, Addressed: true}
		return bot.Incoming{Chat: ch, Load: func(ctx context.Context, learn func(name, id string)) (feature.Message, bool) {
			if ch.GroupID != "" && by.Name != "" {
				learn(by.Name, by.ID)
			}
			return msg, true
		}}
	}
	go c.respond(ix, e.ID, 6)
	return nil
}

// respond acknowledges an interaction: kind 6 with nothing shown, 5 with
// "thinking…".
func (c *Client) respond(ix *interaction, interactionID string, kind int) {
	defer close(ix.acked)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.call(ctx, http.MethodPost, "/interactions/"+interactionID+"/"+ix.token+"/callback", map[string]int{"type": kind}, nil); err != nil {
		logx.Warnf("discord: acknowledging an interaction failed: %v", err)
	}
}

// followUp shows text to whoever pressed a button, as a reply only they
// see, once the press is acknowledged: Discord refuses it before.
func (c *Client) followUp(ix *interaction, text string) {
	ix.acknowledged()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.call(ctx, http.MethodPost, "/webhooks/"+ix.app+"/"+ix.token, map[string]any{"content": txt.Clamp(text, maxChars-1), "flags": 64}, nil); err != nil {
		logx.Warnf("discord: answering a press failed: %v", err)
	}
}

// await keeps a slash command's interaction for the reply to it, beside
// any others pending in the chat, and takes its "thinking…" away if no
// reply comes in time, such as for a command the core never answers.
func (c *Client) await(ch feature.Chat, ix *interaction) {
	key := ch.Key()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.asked[key] == nil {
		c.asked[key] = map[int64]*interaction{}
	}
	if old := c.asked[key][ix.id]; old != nil { // the same interaction again
		old.timer.Stop()
	}
	c.asked[key][ix.id] = ix
	ix.timer = time.AfterFunc(c.pending, func() {
		c.mu.Lock()
		mine := c.asked[key][ix.id] == ix
		if mine {
			c.drop(key, ix.id)
		}
		c.mu.Unlock()
		if mine {
			c.unthink(ix)
		}
	})
}

// drop forgets the slash command msgID pending in the chat key and
// returns it, or nil, under c.mu.
func (c *Client) drop(key string, msgID int64) *interaction {
	ix := c.asked[key][msgID]
	if ix == nil {
		return nil
	}
	if delete(c.asked[key], msgID); len(c.asked[key]) == 0 {
		delete(c.asked, key)
	}
	ix.timer.Stop()
	return ix
}

// Answering notes that the core is answering message msg in chat, so
// Send fills that message's "thinking…" if it is a slash command, and no
// other's.
func (c *Client) Answering(ch feature.Chat, msg int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if msg == 0 {
		delete(c.answering, ch.Key())
		return
	}
	c.answering[ch.Key()] = msg
}

// Answered notes that the core is done answering in chat: a slash
// command it answered with nothing has its "thinking…" taken away now.
func (c *Client) Answered(ch feature.Chat) {
	key := ch.Key()
	c.mu.Lock()
	ix := c.drop(key, c.answering[key])
	delete(c.answering, key)
	c.mu.Unlock()
	if ix != nil {
		go c.unthink(ix)
	}
}

// unthink deletes a slash command's "thinking…".
func (c *Client) unthink(ix *interaction) {
	ix.acknowledged()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.call(ctx, http.MethodDelete, "/webhooks/"+ix.app+"/"+ix.token+"/messages/@original", nil, nil); err != nil {
		logx.Debugf("discord: taking away a slash command's answer failed: %v", err)
	}
}

// take is the slash command the core is answering in chat, now taken, or
// nil: there is none, or its token no longer works.
func (c *Client) take(ch feature.Chat) *interaction {
	c.mu.Lock()
	msgID := c.answering[ch.Key()]
	c.mu.Unlock()
	ix, _ := c.takeID(ch, msgID)
	return ix
}

// takeID is the slash command msgID pending in chat, now taken, or nil if
// its token no longer works; found says it was pending.
func (c *Client) takeID(ch feature.Chat, msgID int64) (ix *interaction, found bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ix = c.drop(ch.Key(), msgID); ix == nil {
		return nil, false
	}
	if time.Since(ix.at) > tokenLife {
		return nil, true
	}
	return ix, true
}

// acknowledged waits, at most 5s, for ix to be acknowledged, as its answer
// can't replace "thinking…" before then.
func (ix *interaction) acknowledged() {
	select {
	case <-ix.acked:
	case <-time.After(5 * time.Second):
	}
}

// noteHook remembers that a slash command's reply is message msgID, which
// only its interaction's token can edit, while the token works.
func (c *Client) noteHook(msgID int64, ix *interaction) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, old := range c.hooks {
		if time.Since(old.at) > tokenLife {
			delete(c.hooks, k)
		}
	}
	c.hooks[msgID] = ix
}

// hook is the interaction message msgID replied to, if its token still
// works.
func (c *Client) hook(msgID int64) *interaction {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ix := c.hooks[msgID]; ix != nil && time.Since(ix.at) < tokenLife {
		return ix
	}
	return nil
}

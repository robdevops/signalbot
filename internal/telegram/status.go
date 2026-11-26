package telegram

import (
	"context"
	"strconv"
	"time"
)

// The bot's own status in a group: Telegram sends a group's reactions only
// to its admins, so a group where the bot isn't one is warned of, once.
const (
	maxStatus       = 2 * maxGroups    // groups whose status is remembered
	maxStatusChecks = 4                // status checks under way at once
	statusRetry     = 10 * time.Minute // after a check that failed
)

// checkStatus asks, off the reader, for the bot's own status in group the
// first time reactions may matter there (a message while a feature has an
// event hook), and warns if it isn't an admin, which it would need to be
// to see reactions. Each group is asked once, until the bot's own
// membership there changes, or again after statusRetry if the check
// failed; one that finds every slot taken is asked at a later message.
func (c *Client) checkStatus(group string) {
	self, err := strconv.ParseInt(c.selfID(), 10, 64)
	if err != nil || !c.watch.Load() {
		return
	}
	now := time.Now()
	c.mu.Lock()
	if t, ok := c.status[group]; ok && (t.IsZero() || now.Before(t)) {
		c.mu.Unlock()
		return
	}
	if len(c.status) >= maxStatus {
		for g := range c.status { // forget one: at worst it is asked again
			delete(c.status, g)
			break
		}
	}
	c.status[group] = time.Time{}
	c.mu.Unlock()
	select {
	case c.statusSlots <- struct{}{}:
	default:
		c.mu.Lock()
		delete(c.status, group)
		c.mu.Unlock()
		return
	}
	ctx := context.Background()
	if p := c.runCtx.Load(); p != nil {
		ctx = *p
	}
	c.statusWG.Add(1)
	go func() {
		defer c.statusWG.Done()
		defer func() { <-c.statusSlots }()
		defer log.Recover("checking the bot's status in a group")
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		var m struct {
			Status string `json:"status"`
		}
		if err := c.call(ctx, "getChatMember", map[string]any{"chat_id": group, "user_id": self}, &m); err != nil {
			log.Debugf("checking the bot's status in group %s failed: %v", group, err)
			c.mu.Lock()
			if t, ok := c.status[group]; ok && t.IsZero() {
				c.status[group] = time.Now().Add(statusRetry)
			}
			c.mu.Unlock()
			return
		}
		if m.Status != "administrator" && m.Status != "creator" {
			log.Warnf("reactions in group %s can't be seen: make the bot an admin of the group (Telegram sends reactions only to admins)", group)
		}
	}()
}

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/kit/logx/logxtest"

	"github.com/robdevops/signalbot/internal/signal"
)

// A group message that isn't for the bot is kept as context, never logged,
// and reaches the model the next time someone addresses the bot.
func TestGroupChatterIsContext(t *testing.T) {
	b, ai, _, sent := photoBot(t, false)
	b.cfg.GroupContext = true
	logs := logxtest.Capture(t)
	b.Handle(parse(t, strings.Replace(groupPlain, "Group test", "the moon is made of cheese", 1)))
	if h := b.getHistory("g:" + group); len(h) != 1 || h[0].Content != "Alice: the moon is made of cheese" || h[0].Expires.IsZero() {
		t.Fatalf("history %+v", h)
	}
	if logs.String() != "" {
		t.Fatalf("chatter was logged: %q", logs.String())
	}
	b.Handle(parse(t, strings.Replace(groupMention, "what is up", "what do you think?", 1)))
	waitSent(t, sent)
	ai.mu.Lock()
	defer ai.mu.Unlock()
	if len(ai.msgs) != 2 || ai.msgs[0].Content != "Alice: the moon is made of cheese" || ai.msgs[1].Content != "Alice: what do you think?" {
		t.Fatalf("model got %+v", ai.msgs)
	}
}

// Photos and stickers in chatter are noted, never fetched; a message with
// nothing to keep (a reaction, a group update) adds nothing.
func TestGroupChatterNotes(t *testing.T) {
	b, _, params, _ := photoBot(t, true)
	b.cfg.GroupContext = true
	photo := strings.Replace(dmPhoto, `"attachments"`, `"groupInfo":{"groupId":"`+group+`","type":"DELIVER"},"attachments"`, 1)
	b.Handle(parse(t, photo))
	b.Handle(parse(t, `{"envelope":{"source":"`+alice+`","sourceUuid":"`+alice+`","sourceName":"Alice","dataMessage":{"message":null,"groupInfo":{"groupId":"`+group+`","type":"UPDATE"}}},"account":"`+self+`"}`))
	h := b.getHistory("g:" + group)
	if len(h) != 1 || h[0].Content != "Alice: [image]" {
		t.Fatalf("history %+v", h)
	}
	time.Sleep(50 * time.Millisecond)
	if len(params) != 0 {
		t.Fatal("an image in chatter was fetched")
	}
}

// With GROUP_CONTEXT off, chatter is dropped untouched, as before.
func TestGroupContextOff(t *testing.T) {
	b, _, _, _ := photoBot(t, false)
	b.cfg.GroupContext = false
	b.Handle(parse(t, groupPlain))
	if h := b.getHistory("g:" + group); len(h) != 0 {
		t.Fatalf("chatter kept with GROUP_CONTEXT off: %+v", h)
	}
}

// However many messages a chat holds, it has at most one expiry timer, and
// it is set for the earliest message to expire.
func TestOneEraserPerChat(t *testing.T) {
	b := NewBot(context.Background(), testCfg(), signal.New("x"), nil)
	b.cfg.HistoryTurns = 100
	now := time.Now()
	for i := 0; i < 50; i++ {
		b.addHistory("g:x", ChatMsg{Role: "user", Content: "m", Expires: now.Add(time.Hour + time.Duration(i)*time.Minute)})
	}
	b.addHistory("g:x", ChatMsg{Role: "user", Content: "soon", Expires: now.Add(30 * time.Millisecond)})
	b.mu.Lock()
	n, at := len(b.erasers), b.erasers["g:x"].at
	b.mu.Unlock()
	if n != 1 || !at.Equal(now.Add(30*time.Millisecond)) {
		t.Fatalf("%d timers, next at %v", n, at.Sub(now))
	}
	time.Sleep(150 * time.Millisecond)
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.history["g:x"]) != 50 || !b.erasers["g:x"].at.Equal(now.Add(time.Hour)) {
		t.Fatalf("after the first expiry: %d messages, next at %v", len(b.history["g:x"]), b.erasers["g:x"].at.Sub(now))
	}
}

func TestGroupContextConfig(t *testing.T) {
	t.Setenv("SIGNALCLI_SOCKET", "/x")
	t.Setenv("OPENROUTER_API_KEY", "k")
	t.Setenv("GROUP_CONTEXT", "")
	if c, _ := LoadConfig(); !c.GroupContext {
		t.Error("GROUP_CONTEXT must default to on")
	}
	t.Setenv("GROUP_CONTEXT", "false")
	if c, _ := LoadConfig(); c.GroupContext {
		t.Error("GROUP_CONTEXT=false must turn it off")
	}
}

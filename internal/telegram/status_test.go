package telegram

import (
	"strings"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/feature"
)

const reactTo500 = `"message_id":500,"user":{"id":1111111,"first_name":"Alice"},"old_reaction":[],"new_reaction":[{"type":"emoji","emoji":"👍"}]`

// ours is the log's lines but the roster's, which name a new group.
func ours(log string) string {
	var b strings.Builder
	for l := range strings.Lines(log) {
		if !strings.Contains(l, "the bot is in a new group") {
			b.WriteString(l)
		}
	}
	return b.String()
}

// Each reaction the reader gets is logged at debug, with nothing about it:
// no emoji, person, chat or name.
func TestReactionLogged(t *testing.T) {
	buf := captureLogs(t)
	f := newFake(t)
	c := connected(f.client())
	c.event(parse(t, `{"update_id":1,"message_reaction":{`+inGroup+`,`+reactTo500+`}}`)) // no hook: still logged
	c.Watch(bot.Watch{Events: true})
	c.event(parse(t, `{"update_id":2,"message_reaction":{`+inGroup+`,`+reactTo500+`}}`))
	out := ours(buf.String())
	if n := strings.Count(out, "Telegram: a reaction received\n"); n != 2 {
		t.Fatalf("%d lines: %q", n, out)
	}
	for _, s := range []string{"👍", "1111111", "Alice", groupS, "Test group"} {
		if strings.Contains(out, s) {
			t.Fatalf("%q logged: %q", s, out)
		}
	}
}

// With an event hook, the bot asks its own status in a group the first
// time a message comes from it, off the reader, and warns once if it isn't
// an admin there, as it then can't see reactions. Its membership changing
// asks again; an admin, or a bot with no event hook, isn't warned.
func TestStatusWarning(t *testing.T) {
	buf := captureLogs(t)
	f := newFake(t)
	c := connected(f.client())
	msg := func(n int) {
		c.event(parse(t, `{"update_id":1,"message":{"message_id":`+string(rune('0'+n))+`,`+fromA+`,`+inGroup+`,"text":"chatter"}}`))
	}
	msg(1)
	time.Sleep(50 * time.Millisecond)
	if n := len(f.callsTo("getChatMember")); n != 0 {
		t.Fatalf("%d lookups with no event hook", n)
	}
	c.Watch(bot.Watch{Events: true})
	msg(2)
	msg(3)
	waitCalls(t, f, "getChatMember", 1)
	c.statusWG.Wait()
	p := f.callsTo("getChatMember")
	if len(p) != 1 || p[0].Params["chat_id"] != groupS || p[0].Params["user_id"] != float64(botID) {
		t.Fatalf("getChatMember %v", p)
	}
	const warn = "Telegram: reactions in group " + groupS + " can't be seen: make the bot an admin of the group (Telegram sends reactions only to admins)\n"
	if n := strings.Count(buf.String(), warn); n != 1 {
		t.Fatalf("%d warnings: %q", n, buf.String())
	}
	if strings.Contains(ours(buf.String()), "Test group") {
		t.Fatalf("a name logged: %q", buf.String())
	}

	// Made an admin: asked again, and not warned.
	f.mu.Lock()
	f.results["getChatMember"] = `{"status":"administrator"}`
	f.mu.Unlock()
	c.event(parse(t, `{"update_id":2,"my_chat_member":{`+inGroup+`,"from":{"id":1111111,"first_name":"Alice"},"old_chat_member":{"status":"member"},"new_chat_member":{"status":"administrator","user":{"id":777000}}}}`))
	msg(4)
	waitCalls(t, f, "getChatMember", 2)
	c.statusWG.Wait()
	if n := strings.Count(buf.String(), warn); n != 1 {
		t.Fatalf("%d warnings: %q", n, buf.String())
	}

	// A check that fails isn't kept: it is asked again later.
	f.mu.Lock()
	f.errs["getChatMember"] = `{"ok":false,"error_code":500,"description":"Internal"}`
	f.mu.Unlock()
	c.event(parse(t, `{"update_id":3,"my_chat_member":{`+inGroup+`,"from":{"id":1111111,"first_name":"Alice"},"old_chat_member":{"status":"administrator"},"new_chat_member":{"status":"member","user":{"id":777000}}}}`))
	msg(5)
	waitCalls(t, f, "getChatMember", 3)
	c.statusWG.Wait()
	msg(6)
	time.Sleep(50 * time.Millisecond)
	if n := len(f.callsTo("getChatMember")); n != 3 {
		t.Fatalf("%d lookups: retried at once", n)
	}
	c.mu.Lock()
	c.status[groupS] = time.Now().Add(-time.Second)
	c.mu.Unlock()
	f.mu.Lock()
	f.results["getChatMember"] = `{"status":"member"}`
	f.mu.Unlock()
	msg(7)
	waitCalls(t, f, "getChatMember", 4)
	c.statusWG.Wait()
	if n := strings.Count(buf.String(), warn); n != 2 {
		t.Fatalf("%d warnings: %q", n, buf.String())
	}
}

// The groups whose status is remembered are bounded.
func TestStatusBounded(t *testing.T) {
	f := newFake(t)
	c := connected(f.client())
	c.Watch(bot.Watch{Events: true})
	c.mu.Lock()
	for i := range maxStatus {
		c.status[string(rune(i))] = time.Time{}
	}
	c.mu.Unlock()
	c.checkStatus(groupS)
	c.statusWG.Wait()
	c.mu.Lock()
	n := len(c.status)
	c.mu.Unlock()
	if n != maxStatus {
		t.Fatalf("%d remembered", n)
	}
	if len(f.callsTo("getChatMember")) != 1 {
		t.Fatal("not asked")
	}
}

// A reaction doesn't say which forum topic it is in, so one to the bot's
// own message is filed under the topic it was sent to; any other stays
// the group's.
func TestReactionTopic(t *testing.T) {
	f := newFake(t)
	c := connected(f.client())
	c.Watch(bot.Watch{Events: true})
	f.results["getChatMember"] = `{"status":"administrator"}`
	c.Send(feature.Chat{GroupID: groupS + "/7"}, "hello", nil, nil, 0, nil, feature.Markup{}) // message 500, in topic 7
	r, _ := c.event(parse(t, `{"update_id":1,"message_reaction":{`+inGroup+`,`+reactTo500+`}}`))
	if e := r.(bot.Happened).Event; e.Chat.GroupID != groupS+"/7" || !e.Reaction.Bot || e.Reaction.Sent != 500 {
		t.Fatalf("reaction %+v %+v", e, e.Reaction)
	}
	r, _ = c.event(parse(t, `{"update_id":2,"message_reaction":{`+inGroup+`,"message_id":42,"user":{"id":1111111,"first_name":"Alice"},"old_reaction":[],"new_reaction":[{"type":"emoji","emoji":"👍"}]}}`))
	if e := r.(bot.Happened).Event; e.Chat.GroupID != groupS || e.Reaction.Bot {
		t.Fatalf("reaction %+v %+v", e, e.Reaction)
	}
	f.results["sendMessage"] = `{"message_id":501}`
	c.Send(feature.Chat{GroupID: groupS}, "hello", nil, nil, 0, nil, feature.Markup{}) // outside a topic
	r, _ = c.event(parse(t, `{"update_id":3,"message_reaction":{`+inGroup+`,"message_id":501,"user":{"id":1111111,"first_name":"Alice"},"old_reaction":[],"new_reaction":[{"type":"emoji","emoji":"👍"}]}}`))
	if e := r.(bot.Happened).Event; e.Chat.GroupID != groupS || !e.Reaction.Bot {
		t.Fatalf("reaction %+v %+v", e, e.Reaction)
	}
}

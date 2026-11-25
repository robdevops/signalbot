package telegram

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/allow"
)

// rosterClient is a connected client keeping its roster in dir.
func rosterClient(f *fakeAPI, dir string) *Client {
	c := newClient(f.srv.URL, token, dir, allow.Parse("all"), nil)
	f.t.Cleanup(c.rosterWG.Wait)
	return connected(c)
}

// savedRoster is what dir's telegram-groups.json holds once c's saves are
// done.
func savedRoster(t *testing.T, c *Client, dir string) map[string]knownGroup {
	t.Helper()
	c.rosterWG.Wait()
	data, err := os.ReadFile(filepath.Join(dir, "telegram-groups.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r map[string]knownGroup
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

const addedToGroup = `{"update_id":1,"my_chat_member":{` + inGroup + `,` + fromA + `,"old_chat_member":{"status":"left"},"new_chat_member":{"status":"member","user":{"id":777000}}}}`

// The bot added to a group saves it, with its title and the day, and warns
// once, with its title and ID; its messages after that change nothing but
// a new title, kept as Telegram gives it.
func TestRosterLearnsFromMembership(t *testing.T) {
	logs := captureLogs(t)
	dir := t.TempDir()
	c := rosterClient(newFake(t), dir)
	c.event(parse(t, addedToGroup))
	r := savedRoster(t, c, dir)
	if g := r[groupS]; len(r) != 1 || g.Title != "Test group" || g.Since != time.Now().Format(time.DateOnly) {
		t.Fatalf("saved %+v", r)
	}
	c.event(parse(t, `{"update_id":2,"message":{"message_id":10,`+fromA+`,`+inGroup+`,"text":"hi"}}`))
	c.event(parse(t, `{"update_id":3,"message":{"message_id":11,`+fromA+`,"chat":{"id":-1001234567890,"type":"supergroup","title":"Renamed \\ group"},"text":"hi"}}`))
	if g := savedRoster(t, c, dir)[groupS]; g.Title != `Renamed \ group` {
		t.Fatalf("title %q", g.Title)
	}
	if n := strings.Count(logs.String(), "the bot is in a new group"); n != 1 || !strings.Contains(logs.String(), "Telegram: the bot is in a new group: Test group (-1001234567890)") {
		t.Fatalf("%d warnings:\n%s", n, logs)
	}
	if strings.Contains(logs.String(), "Renamed") {
		t.Fatalf("a title change was logged:\n%s", logs)
	}
}

// A message, an edit, a reaction or a press from a group not known adds it
// too; a DM adds nothing.
func TestRosterLearnsFromUpdates(t *testing.T) {
	logs := captureLogs(t)
	dir := t.TempDir()
	c := rosterClient(newFake(t), dir)
	c.event(parse(t, `{"update_id":1,"message":{"message_id":10,`+fromA+`,`+inDM+`,"text":"hi"}}`))
	c.rosterWG.Wait()
	if _, err := os.Stat(filepath.Join(dir, "telegram-groups.json")); err == nil {
		t.Fatal("a DM was saved")
	}
	for i, u := range []string{
		`{"update_id":2,"message":{"message_id":10,` + fromA + `,"chat":{"id":-100111,"type":"supergroup","title":"Alpha"},"text":"hi"}}`,
		`{"update_id":3,"edited_message":{"message_id":10,` + fromA + `,"chat":{"id":-100222,"type":"group","title":"Beta"},"text":"hi","edit_date":5}}`,
		`{"update_id":4,"message_reaction":{"chat":{"id":-100333,"type":"supergroup","title":"Gamma"},"message_id":3,"user":{"id":1111111,"first_name":"Alice"},"new_reaction":[{"type":"emoji","emoji":"👍"}]}}`,
		`{"update_id":5,"callback_query":{"id":"q","from":{"id":1111111,"first_name":"Alice"},"message":{"message_id":3,"chat":{"id":-100444,"type":"supergroup","title":"Delta"}},"data":"x"}}`,
	} {
		c.event(parse(t, u))
		if r := savedRoster(t, c, dir); len(r) != i+1 {
			t.Fatalf("after %d: %+v", i, r)
		}
	}
	if n := strings.Count(logs.String(), "the bot is in a new group"); n != 4 {
		t.Fatalf("%d warnings:\n%s", n, logs)
	}
}

// At startup, after getMe, the log lists the saved groups by title, each
// made safe and cut to 40 characters, a backslash shown as "/", or says
// there are none.
func TestRosterAtStartup(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	empty := newClient(f.srv.URL, token, t.TempDir(), allow.Parse("all"), nil)
	if err := empty.hello(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "Telegram groups the bot knows of: none") {
		t.Fatalf("no groups:\n%s", logs)
	}
	dir := t.TempDir()
	saved := `{"-100222": {"title": "beta\\", "since": "2026-01-02"}, "-100111": {"title": "Alpha\nline", "since": "2026-01-01"},
		"-100333": {"title": "` + strings.Repeat("x", 50) + `", "since": "2026-01-03"}, "not-an-id": {"title": "Bob"}}`
	os.WriteFile(filepath.Join(dir, "telegram-groups.json"), []byte(saved), 0o600)
	c := newClient(f.srv.URL, token, dir, allow.Parse("all"), nil)
	if err := c.hello(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := "Telegram groups the bot knows of (3): Alpha line (-100111), beta/ (-100222), " + strings.Repeat("x", 40) + "… (-100333)"
	if !strings.Contains(logs.String(), want) || strings.Contains(logs.String(), "Bob") {
		t.Fatalf("want %q in\n%s", want, logs)
	}
	if strings.Index(logs.String(), "connected to Telegram") > strings.Index(logs.String(), want) {
		t.Fatalf("listed before getMe:\n%s", logs)
	}
}

// The bot removed from a group, or leaving it, drops it; so does a send
// refused because the bot was kicked.
func TestRosterRemoval(t *testing.T) {
	captureLogs(t)
	dir := t.TempDir()
	f := newFake(t)
	c := rosterClient(f, dir)
	c.event(parse(t, addedToGroup))
	c.event(parse(t, `{"update_id":2,"message":{"message_id":10,`+fromA+`,"chat":{"id":-100111,"type":"supergroup","title":"Alpha"},"text":"hi"}}`))
	c.event(parse(t, `{"update_id":3,"my_chat_member":{`+inGroup+`,`+fromA+`,"old_chat_member":{"status":"member"},"new_chat_member":{"status":"kicked","user":{"id":777000}}}}`))
	if r := savedRoster(t, c, dir); len(r) != 1 || r["-100111"].Title != "Alpha" {
		t.Fatalf("after the kick: %+v", r)
	}
	// The service message saying the bot left, after its my_chat_member,
	// doesn't add the group back.
	c.event(parse(t, `{"update_id":4,"message":{"message_id":12,`+fromA+`,`+inGroup+`,"left_chat_member":{"id":777000,"is_bot":true,"first_name":"Bot"}}}`))
	c.event(parse(t, `{"update_id":5,"edited_message":{"message_id":9,`+fromA+`,`+inGroup+`,"text":"hi","edit_date":5}}`))
	if r := savedRoster(t, c, dir); len(r) != 1 {
		t.Fatalf("after the bot's leaving message: %+v", r)
	}
	f.errs["sendMessage"] = `{"ok":false,"error_code":403,"description":"Forbidden: bot was kicked from the supergroup chat"}`
	c.Send(feature.Chat{GroupID: "-100111"}, "hi", nil, nil, 0, nil, feature.Markup{})
	if r := savedRoster(t, c, dir); len(r) != 0 {
		t.Fatalf("after the refused send: %+v", r)
	}
}

// The service message saying the bot left, coming before its
// my_chat_member, drops the group; someone else leaving doesn't.
func TestRosterLeftMessage(t *testing.T) {
	captureLogs(t)
	dir := t.TempDir()
	c := rosterClient(newFake(t), dir)
	c.event(parse(t, addedToGroup))
	c.event(parse(t, `{"update_id":2,"message":{"message_id":12,`+fromA+`,`+inGroup+`,"left_chat_member":{"id":1111111,"first_name":"Alice"}}}`))
	if r := savedRoster(t, c, dir); len(r) != 1 {
		t.Fatalf("after Alice left: %+v", r)
	}
	c.event(parse(t, `{"update_id":3,"message":{"message_id":13,`+fromA+`,`+inGroup+`,"left_chat_member":{"id":777000,"is_bot":true,"first_name":"Bot"}}}`))
	if r := savedRoster(t, c, dir); len(r) != 0 {
		t.Fatalf("after the bot left: %+v", r)
	}
}

// A group that became a supergroup moves to its new ID, keeping the day it
// was first seen, without a new-group warning, whichever side says so
// first.
func TestRosterMigration(t *testing.T) {
	logs := captureLogs(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "telegram-groups.json"), []byte(`{"-111": {"title": "Alpha", "since": "2026-01-01"}}`), 0o600)
	c := rosterClient(newFake(t), dir)
	c.event(parse(t, `{"update_id":2,"message":{"message_id":11,`+fromA+`,"chat":{"id":-111,"type":"group","title":"Alpha"},"migrate_to_chat_id":-100999}}`))
	c.event(parse(t, `{"update_id":3,"message":{"message_id":1,`+fromA+`,"chat":{"id":-100999,"type":"supergroup","title":"Alpha"},"migrate_from_chat_id":-111}}`))
	if r := savedRoster(t, c, dir); len(r) != 1 || r["-100999"].Since != "2026-01-01" {
		t.Fatalf("after migrate_to: %+v", r)
	}
	// The new supergroup's first message first: it moves the old one too.
	os.WriteFile(filepath.Join(dir, "telegram-groups.json"), []byte(`{"-222": {"title": "Beta", "since": "2026-01-02"}}`), 0o600)
	c = rosterClient(newFake(t), dir)
	c.event(parse(t, `{"update_id":4,"message":{"message_id":1,`+fromA+`,"chat":{"id":-100888,"type":"supergroup","title":"Beta"},"migrate_from_chat_id":-222}}`))
	c.event(parse(t, `{"update_id":5,"message":{"message_id":12,`+fromA+`,"chat":{"id":-222,"type":"group","title":"Beta"},"migrate_to_chat_id":-100888}}`))
	if r := savedRoster(t, c, dir); len(r) != 1 || r["-100888"].Since != "2026-01-02" {
		t.Fatalf("after migrate_from: %+v", r)
	}
	if strings.Contains(logs.String(), "the bot is in a new group") {
		t.Fatalf("warned:\n%s", logs)
	}
}

// A file that can't be parsed is warned about, without its content, and
// the bot starts knowing of no groups; the next change replaces it.
func TestRosterCorrupt(t *testing.T) {
	logs := captureLogs(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "telegram-groups.json")
	os.WriteFile(path, []byte(`{"-100111": {"title": "Alice's secret`), 0o600)
	c := rosterClient(newFake(t), dir)
	if !strings.Contains(logs.String(), path+" can't be read, so the bot starts knowing of no groups") || strings.Contains(logs.String(), "secret") {
		t.Fatalf("log:\n%s", logs)
	}
	c.logRoster()
	if !strings.Contains(logs.String(), "knows of: none") {
		t.Fatalf("log:\n%s", logs)
	}
	c.event(parse(t, addedToGroup))
	if r := savedRoster(t, c, dir); len(r) != 1 {
		t.Fatalf("saved %+v", r)
	}
}

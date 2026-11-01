package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/feature/featuretest"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

const (
	alice = "u-alice"
	bob   = "u-bob"
)

// world is the feature on a fake host, in a state directory of its own, at
// a time the test sets.
type world struct {
	t    *testing.T
	h    *featuretest.Host
	dir  string
	now  time.Time
	logs *logx.Buffer
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{t: t, h: featuretest.New(), dir: t.TempDir(), now: date("2030-01-15T12:00"), logs: &logx.Buffer{}}
	w.h.Bot = feature.Info{Name: "Botty"}
	w.h.Settings["STATE_DIRECTORY"] = w.dir
	w.h.Shared = []feature.Group{{ID: "g-family", Name: "🏡 Family 🎉"}, {ID: "g-work", Name: "Work"}}
	t.Cleanup(logx.SetOutput(w.logs, w.logs, true))
	return w
}

func (w *world) register() error { return register(w.h, time.UTC, func() time.Time { return w.now }) }

func (w *world) start() *world {
	w.t.Helper()
	if err := w.register(); err != nil {
		w.t.Fatal(err)
	}
	return w
}

// dm is what Alice's DM to the bot says back.
func (w *world) dm(text string) string {
	w.t.Helper()
	a := w.h.Message(context.Background(), feature.Message{Chat: feature.Chat{User: alice}, Sender: alice, Name: "Alice", Text: text, Addressed: true})
	if a.ToolChoice != "" || a.Context != "" {
		w.t.Fatalf("%q: got %+v", text, a)
	}
	return a.Answer
}

// saved is the schedules file.
func (w *world) saved() []entry {
	w.t.Helper()
	data, err := os.ReadFile(filepath.Join(w.dir, fileName))
	if err != nil {
		w.t.Fatal(err)
	}
	var f struct{ Schedules []entry }
	if err := json.Unmarshal(data, &f); err != nil {
		w.t.Fatal(err)
	}
	return f.Schedules
}

func (w *world) write(es ...entry) {
	w.t.Helper()
	data, _ := json.Marshal(map[string]any{"schedules": es})
	if err := os.WriteFile(filepath.Join(w.dir, fileName), data, 0o600); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) dues() []string {
	var out []string
	for _, tm := range w.h.Timers() {
		if tm.Name != timerName {
			w.t.Errorf("timer named %q", tm.Name)
		}
		out = append(out, tm.Due.UTC().Format("2006-01-02T15:04"))
	}
	return out
}

func (w *world) fire() {
	w.t.Helper()
	if !w.h.Fire(context.Background(), timerName) {
		w.t.Fatal("no timer set")
	}
}

func (w *world) noTextLogged(texts ...string) {
	w.t.Helper()
	for _, s := range texts {
		if strings.Contains(w.logs.String(), s) {
			w.t.Errorf("%q logged:\n%s", s, w.logs)
		}
	}
}

func equal(t *testing.T, what string, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s:\n got %q\nwant %q", what, got, want)
	}
}

func TestParse(t *testing.T) {
	for _, c := range []struct{ in, text, start, every, group string }{
		{`"Hi all" 2030-01-31T09:00`, "Hi all", "2030-01-31T09:00:00", "", ""},
		{`“Hi all” 2030-01-31T09:00 every DAY`, "Hi all", "2030-01-31T09:00:00", "day", ""},
		{`”Hi all” 2030-01-31T09:00 family`, "Hi all", "2030-01-31T09:00:00", "", "family"},
		{`"/forecast Paris"  2030-01-31T09:00Z every week  My  big   group `, "/forecast Paris", "2030-01-31T09:00:00Z", "week", "My  big   group"},
		{`"she said "hi" then “bye”" 2030-01-31T09:00 every month`, `she said "hi" then “bye”`, "2030-01-31T09:00:00", "month", ""},
		{`"a" 2030-01-31T09:00 everyone`, "a", "2030-01-31T09:00:00", "", "everyone"},
	} {
		r, problem := parse(c.in, time.UTC)
		if problem != "" || r.text != c.text || r.start != c.start || r.every != c.every || r.group != c.group {
			t.Errorf("%s: got %+v %q", c.in, r, problem)
		}
	}
	for _, c := range []struct{ in, problem string }{
		{`Hi all 2030-01-31T09:00`, "Put the message in quotes"},
		{`"Hi all 2030-01-31T09:00`, "needs a closing quote"},
		{`"Hi all"`, "Add a time after the message"},
		{`"Hi all" tomorrow`, "I couldn't read the time tomorrow"},
		{`"Hi all" 2030-01-31T09:00 every fortnight`, "Say how often"},
		{`"Hi all" 2030-01-31T09:00 every`, "Say how often"},
		{`"  " 2030-01-31T09:00`, "The message is empty"},
		{`"` + strings.Repeat("x", maxText+1) + `" 2030-01-31T09:00`, "too long"},
	} {
		if _, problem := parse(c.in, time.UTC); !strings.Contains(problem, c.problem) {
			t.Errorf("%s: got %q", c.in, problem)
		}
	}
}

// A schedule is made, listed, saved and deleted, all in Alice's DM; the
// group is found ignoring capitals, emoji and symbols.
func TestAddListDel(t *testing.T) {
	w := newWorld(t).start()
	if got := w.dm(`/schedule “Morning all” 2030-02-01T09:00 every day family`); got != "Scheduled for Fri 1 Feb 2030 09:00 UTC, then every day, in 🏡 Family 🎉." {
		t.Fatalf("got %q", got)
	}
	if got := w.dm(`/schedule "/forecast Paris" 2030-03-01T07:30+10:00`); got != "Scheduled for Fri 1 Mar 2030 07:30 +1000, here." {
		t.Fatalf("got %q", got)
	}
	equal(t, "timers", w.dues(), []string{"2030-02-01T09:00", "2030-02-28T21:30"})
	if s := w.saved(); !reflect.DeepEqual(s, []entry{
		{ID: 1, User: alice, Group: "g-family", GroupName: "🏡 Family 🎉", Text: "Morning all", Start: "2030-02-01T09:00:00", Every: "day"},
		{ID: 2, User: alice, Text: "/forecast Paris", Start: "2030-03-01T07:30:00+10:00"},
	}) {
		t.Fatalf("saved %+v", s)
	}

	// Bob's schedules are his own.
	b := w.h.Message(context.Background(), feature.Message{Chat: feature.Chat{User: bob}, Sender: bob, Text: "/schedule list", Addressed: true})
	if !strings.HasPrefix(b.Answer, "You have no schedules.") {
		t.Fatalf("Bob got %q", b.Answer)
	}

	w.now = date("2030-02-03T10:00")
	want := "Your schedules:\n" +
		"1. \"Morning all\" Mon 4 Feb 2030 09:00 UTC, every day, in 🏡 Family 🎉\n" +
		"2. \"/forecast Paris\" Fri 1 Mar 2030 07:30 +1000, here"
	if got := w.dm("/schedule LIST"); got != want {
		t.Fatalf("list:\n%s", got)
	}
	// Alice leaves the group: the schedule stays, marked.
	w.h.Shared = nil
	if got := w.dm("/schedule list"); !strings.Contains(got, "in 🏡 Family 🎉 (you've left it, so it won't post)\n") {
		t.Fatalf("list:\n%s", got)
	}

	if got := w.dm("/schedule del 3"); !strings.HasPrefix(got, "You don't have a schedule 3.") {
		t.Fatalf("got %q", got)
	}
	if got := w.dm("/schedule del one"); !strings.HasPrefix(got, "Say which one") {
		t.Fatalf("got %q", got)
	}
	if got := w.dm("/schedule del 1"); got != `Deleted schedule 1: "Morning all".` {
		t.Fatalf("got %q", got)
	}
	equal(t, "timers", w.dues(), []string{"2030-02-28T21:30"})
	if s := w.saved(); len(s) != 1 || s[0].Text != "/forecast Paris" {
		t.Fatalf("saved %+v", s)
	}
	// The rest keep their numbers.
	if got := w.dm("/schedule list"); !strings.HasPrefix(got, "Your schedules:\n2. \"/forecast Paris\" ") {
		t.Fatalf("list:\n%s", got)
	}
	if got := w.dm("/schedule del 1"); !strings.HasPrefix(got, "Schedule 1 has already run or been deleted.") {
		t.Fatalf("got %q", got)
	}
	if got := w.dm("/schedule"); !strings.Contains(got, usage) || !strings.Contains(got, "my time zone (UTC)") || w.dm("/schedule help") != got ||
		!strings.Contains(got, "+10:00.\n\n- Without a group name") || !strings.HasSuffix(got, "/schedule.\n\nThe current time is "+w.now.Format("2006-01-02T15:04Z07:00")) {
		t.Fatalf("help %q", got)
	}
	w.noTextLogged("Morning all", "forecast Paris")
}

// The group is matched exactly first, then without emoji, symbols or extra
// spaces; no match, or more than one, is refused.
func TestGroupMatching(t *testing.T) {
	w := newWorld(t).start()
	w.h.Shared = []feature.Group{{ID: "g1", Name: "Family"}, {ID: "g2", Name: "🏡 Family"}, {ID: "g3", Name: "Family 🎉"}, {ID: "g4", Name: "Mum's  Book Club!"}}
	add := func(group string) string { return w.dm(`/schedule "hi" 2030-02-01T09:00 ` + group) }
	if got := add("FAMILY"); !strings.HasSuffix(got, ", in Family.") {
		t.Fatalf("got %q", got)
	}
	if got := add("🏡 family"); !strings.HasSuffix(got, ", in 🏡 Family.") {
		t.Fatalf("got %q", got)
	}
	if got := add("mums book club"); !strings.HasSuffix(got, ", in Mum's  Book Club!.") {
		t.Fatalf("got %q", got)
	}
	w.h.Shared = w.h.Shared[1:]
	if got := add("family"); got != "More than one of your groups is called family: 🏡 Family, Family 🎉. Rename one, then try again." {
		t.Fatalf("got %q", got)
	}
	if got := add("Chess"); got != "We aren't in a group called Chess together. Your groups with me: 🏡 Family, Family 🎉, Mum's  Book Club!." {
		t.Fatalf("got %q", got)
	}
	if got := add("🎉"); !strings.HasPrefix(got, "We aren't in a group called 🎉") {
		t.Fatalf("emoji alone matched: %q", got)
	}
	w.h.Shared = nil
	if got := add("Chess"); got != "We aren't in any groups together." {
		t.Fatalf("got %q", got)
	}
	if len(w.saved()) != 3 {
		t.Fatalf("saved %+v", w.saved())
	}
}

func TestRefusals(t *testing.T) {
	w := newWorld(t).start()
	group := feature.Message{Chat: feature.Chat{GroupID: "g-family"}, Sender: alice, Text: `/schedule "hi" 2030-02-01T09:00`, Addressed: true}
	if a := w.h.Message(context.Background(), group); !strings.HasPrefix(a.Answer, "Schedules are made in a DM with me.") {
		t.Fatalf("group got %+v", a)
	}
	if got := w.dm(`/schedule "hi" 2030-01-15T11:59`); got != "That time has passed." {
		t.Fatalf("got %q", got)
	}
	for _, text := range []string{"/schedule list", "/SCHEDULE", "@Botty /schedule del 1", "@signalbot  @botty /schedule help",
		"/timer list", "@Botty /reminder help", "/Reminder", "/job del 1"} {
		if got := w.dm(`/schedule "` + text + `" 2030-02-01T09:00`); got != "A schedule can't make schedules." {
			t.Errorf("%s: got %q", text, got)
		}
	}
	// The bot's names are matched as the core strips them: the longest
	// first, on a word boundary. With the bot called Signal, "@signalbot"
	// is still the bot, not "@Signal" and then "bot".
	w.h.Bot.Name = "Signal"
	for _, text := range []string{"@signalbot /schedule list", "@Signal @SIGNALBOT /schedule help", "@signal\n/schedule"} {
		if got := w.dm(`/schedule "` + text + `" 2030-02-01T09:00 every hour`); got != "A schedule can't make schedules." {
			t.Errorf("%s: got %q", text, got)
		}
	}
	// Not the bot's name, so not stripped, so not a command when it runs.
	for _, text := range []string{"@Signals /schedule list", "@signal_bot /schedule list"} {
		if got := w.dm(`/schedule "` + text + `" 2030-02-01T09:00`); !strings.HasPrefix(got, "Scheduled for ") {
			t.Errorf("%s: got %q", text, got)
		}
	}
	w.h.Bot.Name = "Botty"
	w.h.TimerErr = errors.New("too many timers")
	if got := w.dm(`/schedule "hi" 2030-02-01T09:00`); got != "Sorry, I can't take more schedules right now." {
		t.Fatalf("got %q", got)
	}
	if n := len(w.saved()); n != 2 {
		t.Fatalf("saved %d", n)
	}
	if !strings.Contains(w.logs.String(), "<4>schedule refused: too many timers\n") {
		t.Fatalf("logs:\n%s", w.logs)
	}
	// Not commands of its own: carried on.
	if a := w.h.Message(context.Background(), feature.Message{Chat: feature.Chat{User: alice}, Sender: alice, Text: "/schedules", Addressed: true}); a != (feature.Action{}) {
		t.Fatalf("got %+v", a)
	}
}

// A repeat may start in the past; it first runs at its next time to come.
func TestPastStartOfARepeat(t *testing.T) {
	w := newWorld(t).start()
	if got := w.dm(`/schedule "rent" 2029-10-31T08:00 every month`); got != "Scheduled for Thu 31 Jan 2030 08:00 UTC, then every month, here." {
		t.Fatalf("got %q", got)
	}
	w.now = date("2030-01-31T08:00")
	w.fire()
	equal(t, "timers", w.dues(), []string{"2030-02-28T08:00"})
}

// A one-off posts its text as Alice, in its chat, and is deleted.
func TestOneOffRuns(t *testing.T) {
	w := newWorld(t).start()
	w.dm(`/schedule "see you @Bob" 2030-01-15T13:00 work`)
	w.dm(`/schedule "@Botty how's the weather?" 2030-01-15T14:00`)
	w.now = date("2030-01-15T13:00").Add(time.Second)
	w.fire()
	w.now = date("2030-01-15T14:59") // 59 minutes late: still runs
	w.fire()
	got := w.h.Delivered()
	if !reflect.DeepEqual(got, []feature.Message{
		{Chat: feature.Chat{GroupID: "g-work"}, Sender: alice, Text: "see you @Bob"},
		{Chat: feature.Chat{User: alice}, Sender: alice, Text: "@Botty how's the weather?"},
	}) {
		t.Fatalf("delivered %+v", got)
	}
	if len(w.saved()) != 0 || len(w.h.Timers()) != 0 {
		t.Fatalf("left %+v %+v", w.saved(), w.h.Timers())
	}
	w.noTextLogged("see you @Bob", "the weather")
}

// A schedule's number stays the same, and is never given again, so a
// one-off that runs between /schedule list and /schedule del can't make
// del take the next one.
func TestNumbersDontShift(t *testing.T) {
	w := newWorld(t).start()
	w.dm(`/schedule "soon" 2030-01-15T12:01`)
	w.dm(`/schedule "weekly" 2030-01-16T09:00 every week`)
	if got := w.dm("/schedule list"); !strings.Contains(got, "\n1. \"soon\" ") || !strings.Contains(got, "\n2. \"weekly\" ") {
		t.Fatalf("list:\n%s", got)
	}
	w.now = date("2030-01-15T12:01")
	w.fire() // "soon" runs
	if got := w.dm("/schedule del 1"); got != "Schedule 1 has already run or been deleted. /schedule list shows yours." {
		t.Fatalf("got %q", got)
	}
	if s := w.saved(); len(s) != 1 || s[0].ID != 2 || s[0].Text != "weekly" {
		t.Fatalf("saved %+v", s)
	}

	// After a restart, a new one still gets a number never used.
	w2 := newWorld(t)
	w2.dir, w2.now = w.dir, w.now
	w2.h.Settings["STATE_DIRECTORY"] = w.dir
	w2.start()
	w2.dm(`/schedule "new" 2030-01-17T09:00`)
	if got := w2.dm("/schedule list"); !strings.Contains(got, "\n2. \"weekly\" ") || !strings.Contains(got, "\n3. \"new\" ") {
		t.Fatalf("list:\n%s", got)
	}
	// Bob's numbers are his own.
	b := w2.h.Message(context.Background(), feature.Message{Chat: feature.Chat{User: bob}, Sender: bob, Text: `/schedule "mine" 2030-01-17T09:00`, Addressed: true})
	l := w2.h.Message(context.Background(), feature.Message{Chat: feature.Chat{User: bob}, Sender: bob, Text: "/schedule list", Addressed: true})
	if !strings.HasPrefix(b.Answer, "Scheduled") || !strings.HasPrefix(l.Answer, "Your schedules:\n1. \"mine\" ") {
		t.Fatalf("Bob got %q, %q", b.Answer, l.Answer)
	}
}

// A repeat runs, and sets its next run from the clock. A run it missed,
// with the bot asleep, is skipped, and the next is still from the clock.
func TestRepeatRuns(t *testing.T) {
	w := newWorld(t).start()
	w.dm(`/schedule "stand-up" 2030-01-15T13:00 every hour`)
	w.now = date("2030-01-15T13:00").Add(2 * time.Second)
	w.fire()
	equal(t, "timers", w.dues(), []string{"2030-01-15T14:00"})
	w.now = date("2030-01-15T17:20") // asleep since before 14:00
	w.fire()
	equal(t, "timers", w.dues(), []string{"2030-01-15T18:00"})
	if n := len(w.h.Delivered()); n != 1 {
		t.Fatalf("%d delivered", n)
	}
	if !strings.Contains(w.logs.String(), "<6>a repeating schedule skipped a run missed by 3h20m0s\n") {
		t.Fatalf("logs:\n%s", w.logs)
	}
	// At the cap, it can't be set again, which is logged.
	w.h.TimerErr = errors.New("too many timers")
	w.now = date("2030-01-15T18:00")
	w.fire()
	if len(w.h.Delivered()) != 2 || !strings.Contains(w.logs.String(), "<3>a repeating schedule can't be set again: too many timers\n") {
		t.Fatalf("logs:\n%s", w.logs)
	}
	w.noTextLogged("stand-up")
}

// At startup the saved schedules are set again: a one-off less than an hour
// late runs, an older one is dropped and deleted from the file, and a
// repeat skips to its next time to come. One without a number, or with a
// number another of the user's has, is given the next one.
func TestLoad(t *testing.T) {
	w := newWorld(t)
	w.write(
		entry{User: alice, Text: "recent", Start: "2030-01-15T11:30:00"},
		entry{ID: 2, User: alice, Text: "stale one", Start: "2030-01-15T10:59:00"},
		entry{ID: 2, User: alice, Group: "g-work", GroupName: "Work", Text: "weekly", Start: "2030-01-01T09:00:00Z", Every: "week"},
		entry{User: bob, Text: "", Start: "2030-01-15T11:30:00"}, // malformed
	)
	w.start()
	equal(t, "timers", w.dues(), []string{"2030-01-15T11:30", "2030-01-15T10:59", "2030-01-22T09:00"})
	w.fire()
	w.fire()
	if d := w.h.Delivered(); len(d) != 1 || d[0].Text != "recent" {
		t.Fatalf("delivered %+v", d)
	}
	if s := w.saved(); len(s) != 1 || s[0].Text != "weekly" || s[0].ID != 4 {
		t.Fatalf("saved %+v", s)
	}
	logs := w.logs.String()
	for _, want := range []string{"<3>a saved schedule is malformed and was skipped\n", "<4>a one-off schedule was dropped: it was due 1h1m0s ago\n", "<5>loaded 3 schedules\n"} {
		if !strings.Contains(logs, want) {
			t.Errorf("no %q in:\n%s", want, logs)
		}
	}
	if got := w.dm("/schedule list"); got != "Your schedules:\n4. \"weekly\" Tue 22 Jan 2030 09:00 UTC, every week, in Work" {
		t.Fatalf("list %q", got)
	}
	w.noTextLogged("recent", "stale one", "weekly")
}

// A saved schedule that can't be set at startup is logged, and kept.
func TestLoadAtTheCap(t *testing.T) {
	w := newWorld(t)
	w.write(entry{User: alice, Text: "later", Start: "2030-02-01T09:00:00"})
	w.h.TimerErr = errors.New("too many timers")
	w.start()
	if !strings.Contains(w.logs.String(), "<3>a saved schedule can't be set: too many timers\n") {
		t.Fatalf("logs:\n%s", w.logs)
	}
	if got := w.dm("/schedule list"); !strings.Contains(got, `1. "later"`) {
		t.Fatalf("list %q", got)
	}
	if got := w.dm("/schedule del 1"); !strings.HasPrefix(got, "Deleted") || len(w.saved()) != 0 {
		t.Fatalf("got %q", got)
	}
}

// A schedules file that can't be read stops the bot, rather than being
// overwritten.
func TestUnreadableFile(t *testing.T) {
	w := newWorld(t)
	os.WriteFile(filepath.Join(w.dir, fileName), []byte("{not json"), 0o600)
	if err := w.register(); err == nil || !strings.Contains(err.Error(), "schedules in "+filepath.Join(w.dir, fileName)+" can't be read") {
		t.Fatalf("got %v", err)
	}
}

// Run by hand, the schedules are saved in the working directory, and a
// failed save refuses the schedule.
func TestWorkingDirectory(t *testing.T) {
	w := newWorld(t)
	delete(w.h.Settings, "STATE_DIRECTORY")
	t.Chdir(w.dir)
	w.start()
	w.dm(`/schedule "hi" 2030-02-01T09:00`)
	if len(w.saved()) != 1 {
		t.Fatal("not saved")
	}
	os.Remove(filepath.Join(w.dir, fileName))
	os.Mkdir(filepath.Join(w.dir, fileName), 0o700) // so the rename fails
	if got := w.dm(`/schedule "hi" 2030-02-01T09:00`); got != "Sorry, I couldn't save that schedule: I can't write to my storage. Please let my owner know." || len(w.h.Timers()) != 1 {
		t.Fatalf("got %q, %d timers", got, len(w.h.Timers()))
	}
}

// In Docker, a state directory the bot can't use stops it, saying how to
// fix the host directory's owner.
func TestDockerDirectory(t *testing.T) {
	w := newWorld(t)
	old := dockerDir
	t.Cleanup(func() { dockerDir = old })
	dockerDir = filepath.Join(w.dir, "data")
	w.h.Settings["STATE_DIRECTORY"] = dockerDir
	err := w.register()
	if err == nil || !strings.Contains(err.Error(), "run: chown 65532 /var/lib/private/signalbot") {
		t.Fatalf("got %v", err)
	}
	os.Mkdir(dockerDir, 0o700)
	if err := w.register(); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dockerDir); len(entries) != 0 {
		t.Fatalf("left %v", entries)
	}
}

// Anywhere else, a state directory the bot can't list and write stops it
// at startup, saying to give the systemd unit a StateDirectory, and adds
// nothing. (Tests may run as root, which file modes don't stop, so the
// directories here are missing rather than read-only.)
func TestStateDirectory(t *testing.T) {
	const fix = "add StateDirectory=signalbot to the unit's [Service] section"
	w := newWorld(t)
	missing := filepath.Join(w.dir, "missing")
	w.h.Settings["STATE_DIRECTORY"] = missing + ":" + w.dir
	err := w.register()
	if err == nil || !strings.Contains(err.Error(), "its state directory "+missing+" (") || !strings.Contains(err.Error(), fix) {
		t.Fatalf("got %v", err)
	}
	if len(w.h.IntroLines()) != 0 || len(w.h.ToolGroups()) != 0 || w.dm(`/schedule list`) != "" {
		t.Fatal("registered anyway")
	}

	// Run by hand, the working directory.
	gone := filepath.Join(w.dir, "gone")
	os.Mkdir(gone, 0o700)
	t.Chdir(gone)
	os.Remove(gone)
	delete(w.h.Settings, "STATE_DIRECTORY")
	err = w.register()
	if err == nil || !strings.Contains(err.Error(), "the working directory, as STATE_DIRECTORY isn't set") || !strings.Contains(err.Error(), fix) {
		t.Fatalf("got %v", err)
	}

	os.Mkdir(missing, 0o700)
	w.h.Settings["STATE_DIRECTORY"] = missing
	w.start()
	if entries, _ := os.ReadDir(missing); len(entries) != 0 {
		t.Fatalf("left %v", entries)
	}
}

// /timer, /reminder and /job are the same command as /schedule, and the
// model is told all four, with no tools, for a near-miss such as "/time".
func TestAliases(t *testing.T) {
	w := newWorld(t).start()
	want := []feature.ToolGroup{{Names: []string{"**schedules and reminders**: /schedule, /timer, /reminder, /job"}}}
	if g := w.h.ToolGroups(); !reflect.DeepEqual(g, want) {
		t.Fatalf("tool groups %+v", g)
	}
	for _, cmd := range []string{"/timer", "/reminder", "/job"} {
		if got := w.dm(cmd + ` "hi" 2030-02-01T09:00`); !strings.HasPrefix(got, "Scheduled for ") {
			t.Fatalf("%s: got %q", cmd, got)
		}
	}
	if got := w.dm("/reminder list"); !strings.Contains(got, "1.") || !strings.Contains(got, "3.") {
		t.Fatalf("list: %q", got)
	}
	if got := w.dm("/timer help"); !strings.Contains(got, "/timer, /reminder and /job work the same as /schedule.") {
		t.Fatalf("help: %q", got)
	}
}

// Package schedule is /schedule: a user schedules anything they could post,
// in a DM with the bot or a group they share with it, once or repeating.
// At each run the text acts as if they had just posted it. Schedules are
// saved as JSON and set again when the bot starts.
package schedule

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

const (
	// storeName is the schedules' saved state, schedules.json in the state
	// directory.
	storeName = "schedules"
	// grace is how late a one-off may run, after the bot was down or asleep;
	// later than that it is dropped.
	grace = time.Hour
	// onTime is how late a repeating run may be and still count as on time;
	// later than that it was missed, and is skipped.
	onTime = time.Minute
	// timerName is what the core logs for each run: never the text.
	timerName = "schedule"
	// whenLayout is how a time is shown to the user.
	whenLayout = "Mon 2 Jan 2006 15:04 MST"
)

const usage = `/schedule "<message or /command>" <time> [every hour|day|week|month|quarter|year] [group name]`

// entry is one schedule, as saved.
type entry struct {
	ID        int    `json:"id"`                   // its number in its user's list, never reused
	User      string `json:"user"`                 // who scheduled it, and posts it
	Group     string `json:"group,omitempty"`      // the group's ID; "" for the DM
	GroupName string `json:"group_name,omitempty"` // its name when scheduled, for when the user has left it
	Text      string `json:"text"`
	Start     string `json:"start"` // ISO 8601: without an offset, in the bot's time zone
	Every     string `json:"every,omitempty"`

	start  stamp
	cancel func() // its timer's; nil if none is set
}

// savedFile is the schedules file.
type savedFile struct {
	Schedules []*entry       `json:"schedules"`
	Last      map[string]int `json:"last_id,omitempty"` // the last ID each user was given
}

// scheduler is every user's schedules, in the order they were made.
type scheduler struct {
	h   feature.Host
	db  feature.Store
	loc *time.Location
	now func() time.Time

	mu   sync.Mutex
	all  []*entry
	last map[string]int // the last ID each user was given, so none is reused
}

// Register adds /schedule to h, and sets the saved schedules again. It
// fails, stopping the bot, if the state directory can't be listed and
// written, rather than failing at the first save.
func Register(h feature.Host) error { return register(h, time.Local, time.Now) }

func register(h feature.Host, loc *time.Location, now func() time.Time) error {
	db, err := h.Store(storeName)
	if err != nil {
		return fmt.Errorf("/schedule: %w", err)
	}
	s := &scheduler{h: h, db: db, loc: loc, now: now, last: map[string]int{}}
	if err := s.load(); err != nil {
		return err
	}
	for _, name := range commands {
		if err := feature.Command(h, name, s.command); err != nil {
			return err
		}
	}
	h.IntroLine(`Type "/schedule" for schedules and reminders.`, true) // made only in a DM
	// No model tools, so this names the commands only in the prompt's list
	// of what the bot has, for a near-miss such as "/time" to be pointed here.
	h.Tools(feature.ToolGroup{Names: []string{"**schedules and reminders**: /" + strings.Join(commands, ", /")}})
	return nil
}

// commands are the command and its aliases, all the same command.
var commands = []string{"schedule", "timer", "reminder"}

// verbs are the subcommands, by every word that names one.
var verbs = map[string]string{
	"help": "help",
	"list": "list", "ls": "list",
	"del": "del", "delete": "del", "remove": "del", "cancel": "del", "rm": "del",
}

// load reads the saved schedules and sets their timers. A file that can't
// be read stops the bot, rather than being overwritten by the next save.
func (s *scheduler) load() error {
	var f savedFile
	if _, err := s.db.Load(&f); err != nil {
		return fmt.Errorf("schedules: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if f.Last != nil {
		s.last = f.Last
	}
	type userID struct {
		user string
		id   int
	}
	taken := map[userID]bool{}
	for i, e := range f.Schedules {
		if bad := e.malformed(s.loc); bad != "" {
			logx.Errorf("%s.json: entry %d skipped: %s; it will be removed at the next save", storeName, i+1, bad)
			continue
		}
		if k := (userID{e.User, e.ID}); e.ID > 0 && !taken[k] {
			taken[k] = true
			s.last[e.User] = max(s.last[e.User], e.ID)
		} else {
			e.ID = 0 // given one below, once every ID in use is known
		}
		s.all = append(s.all, e)
	}
	for _, e := range s.all {
		if e.ID == 0 {
			s.last[e.User]++
			e.ID = s.last[e.User]
		}
		if err := s.arm(e, e.first(s.now())); err != nil {
			logx.Errorf("a saved schedule can't be set: %v", err)
		}
	}
	logx.Noticef("loaded %d schedules", len(s.all))
	return nil
}

// malformed says what is wrong with a saved entry, naming the field and
// never its value, and sets its start; "" if nothing is.
func (e *entry) malformed(loc *time.Location) string {
	var ok bool
	switch {
	case e == nil:
		return "empty"
	case e.User == "":
		return "no user"
	case e.Text == "":
		return "no text"
	case e.Every != "" && units[e.Every] == 0:
		return "bad every"
	}
	if e.start, _, ok = parseTime(e.Start, loc); !ok {
		return "bad start"
	}
	return ""
}

// first is e's first run from now: a one-off's own time, past or not, so
// that a late one is judged when it runs; a repeat's next time to come.
func (e *entry) first(now time.Time) time.Time {
	if at := e.start.at(); e.Every == "" || at.After(now) {
		return at
	}
	return next(e.start, e.Every, now)
}

// save writes every schedule, whole. s.mu is held.
func (s *scheduler) save() error { return s.db.Save(savedFile{Schedules: s.all, Last: s.last}) }

// arm sets e's timer for t. s.mu is held.
func (s *scheduler) arm(e *entry, t time.Time) error {
	cancel, err := s.h.At(timerName, t, func(_ context.Context, due time.Time) { s.run(e, due) })
	e.cancel = cancel
	return err
}

// run is e's timer, due at dueAt: it delivers e's text if settle says to.
func (s *scheduler) run(e *entry, dueAt time.Time) {
	if s.settle(e, dueAt) {
		s.h.Deliver(feature.Message{Chat: e.chat(), Sender: e.User, Text: e.Text})
	}
}

// settle settles e's run that was due at dueAt, and reports whether to
// deliver it: a one-off is deleted, and dropped if it is an hour late, its
// owner told by DM; a repeat is set again, worked out from the clock, and
// skips a run it missed.
func (s *scheduler) settle(e *entry, dueAt time.Time) (deliver bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Contains(s.all, e) {
		return false // deleted as it came due
	}
	now := s.now()
	late := now.Sub(dueAt)
	e.cancel = nil
	if e.Every != "" {
		after := now
		if dueAt.After(now) { // the clock went back
			after = dueAt
		}
		if err := s.arm(e, next(e.start, e.Every, after)); err != nil {
			logx.Errorf("a repeating schedule can't be set again: %v", err)
		}
		if late >= onTime {
			logx.Infof("a repeating schedule skipped a run missed by %s", late.Round(time.Second))
			return false
		}
		return true
	}
	s.all = slices.DeleteFunc(s.all, func(x *entry) bool { return x == e })
	if err := s.save(); err != nil {
		logx.Errorf("schedules not saved: %v", err)
	}
	if late >= grace {
		logx.Warnf("a one-off schedule was dropped: it was due %s ago", late.Round(time.Minute))
		s.h.Tell(e.User, "I was offline at "+dueAt.In(s.loc).Format(whenLayout)+", so your reminder \""+
			txt.Clamp(strings.Join(strings.Fields(e.Text), " "), 80)+"\" didn't post.")
		return false
	}
	return true
}

func (e *entry) chat() feature.Chat {
	if e.Group != "" {
		return feature.Chat{GroupID: e.Group}
	}
	return feature.Chat{User: e.User}
}

// command answers /schedule, in a DM only: the core only passes on a DM
// from a sender in SIGNAL_DM_USER_IDS.
func (s *scheduler) command(ctx context.Context, m feature.Message, args string) feature.Action {
	if m.Chat.GroupID != "" {
		return answer("Schedules are made in a DM with me. /schedule help there says how.")
	}
	word, rest := cutField(args)
	switch verbs[strings.ToLower(word)] {
	case "help":
		return answer(s.help())
	case "list":
		return answer(s.list(ctx, m.Sender))
	case "del":
		return answer(s.del(m.Sender, rest))
	}
	q, _ := utf8.DecodeRuneInString(args)
	switch {
	case args == "":
		return answer(s.help())
	case !strings.ContainsRune(quotes, q) && !s.looksLikeASchedule(args):
		name, _, _ := feature.ParseCommand(m.Text)
		return answer("I don't know \"/" + cmp.Or(name, "schedule") + " " + txt.Clamp(word, 30) + "\". " +
			"Try /schedule list, /schedule del <number>, or /schedule help.")
	}
	return answer(s.add(ctx, m, args))
}

// looksLikeASchedule reports whether args, which don't start with a quote,
// hold a time, so that they are a new schedule missing its quotes rather
// than an unknown subcommand.
func (s *scheduler) looksLikeASchedule(args string) bool {
	for w := range strings.FieldsSeq(args) {
		if _, _, ok := parseTime(w, s.loc); ok || isDate(w) {
			return true
		}
	}
	return false
}

// example is the current time, as the help and the errors show a time.
func (s *scheduler) example() string { return s.now().In(s.loc).Format(exampleLayout) }

func answer(text string) feature.Action { return feature.Action{Answer: text} }

func (s *scheduler) help() string {
	example := s.example()
	return strings.Join([]string{
		"Schedule a message, or a /command, to post as if you'd sent it:\n\n" + usage,
		"Copy and edit this one, which has the time now:\n/schedule \"hello\" " + example,
		"- The time is a date and time in my time zone (" + s.now().In(s.loc).Format("MST") +
			"), or with an offset, like " + example + "+10:00.",
		"- Without a group name it's posted here. A message that @mentions me gets my answer.",
		"- A monthly schedule from the 31st runs on the last day of shorter months.",
		"/schedule list: your schedules, numbered",
		"/schedule del <number>: delete one",
		"/timer and /reminder work the same as /schedule.",
	}, "\n\n")
}

// add makes a schedule from args.
func (s *scheduler) add(ctx context.Context, m feature.Message, args string) string {
	r, problem := parse(args, s.loc, s.example())
	if problem != "" {
		return problem
	}
	if s.schedulesItself(m.Chat, r.text) {
		return "A schedule can't make schedules."
	}
	now := s.now()
	e := &entry{User: m.Sender, Text: r.text, Start: r.start, Every: r.every, start: r.at}
	if e.Every == "" && !e.start.at().After(now) {
		return "That time has passed."
	}
	where := "here"
	if r.group != "" {
		g, problem := s.findGroup(ctx, m.Sender, r.group)
		if problem != "" {
			return problem
		}
		e.Group, e.GroupName, where = g.ID, g.Name, "in "+g.Name
	}
	first := e.first(now)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.arm(e, first); err != nil {
		logx.Warnf("schedule refused: %v", err)
		return "Sorry, I can't take more schedules right now."
	}
	s.last[e.User]++
	e.ID = s.last[e.User]
	s.all = append(s.all, e)
	if err := s.save(); err != nil {
		logx.Errorf("schedules not saved: %v", err)
		e.cancel()
		s.all = s.all[:len(s.all)-1]
		s.last[e.User]--
		return "Sorry, I couldn't save that schedule: I can't write to my storage. Please let my owner know."
	}
	repeat := ""
	if e.Every != "" {
		repeat = ", then every " + e.Every
	}
	return "Scheduled for " + first.Format(whenLayout) + repeat + ", " + where + "."
}

// schedulesItself reports whether text is /schedule, even after @mentions
// of the bot, which the core strips when the text is delivered. It matches
// them as the core does: the longest name first, on a word boundary.
func (s *scheduler) schedulesItself(chat feature.Chat, text string) bool {
	names := []string{"@" + s.h.Info().Name, "@signalbot"}
	slices.SortFunc(names, func(a, b string) int { return utf8.RuneCountInString(b) - utf8.RuneCountInString(a) })
	for t := strings.TrimSpace(text); ; {
		rest, ok := "", false
		for _, n := range names {
			if rest, ok = cutAt(t, n); ok {
				break
			}
		}
		if !ok {
			name, _, ok := feature.ParseCommand(t)
			return ok && slices.Contains(commands, name)
		}
		t = strings.TrimSpace(rest)
	}
}

// cutAt is t after a leading @name, matched ignoring capitals and only when
// no letter, digit or '_' follows it.
func cutAt(t, name string) (string, bool) {
	rs, n := []rune(t), utf8.RuneCountInString(name)
	if n < 2 || len(rs) < n || strings.ToLower(string(rs[:n])) != strings.ToLower(name) {
		return "", false
	}
	if len(rs) > n && (unicode.IsLetter(rs[n]) || unicode.IsDigit(rs[n]) || rs[n] == '_') {
		return "", false
	}
	return string(rs[n:]), true
}

// findGroup is the group the user shares with the bot called name, ignoring
// capitals: an exact match, else one ignoring emoji, symbols and extra
// spaces. It returns what is wrong, for the user, if there isn't just one.
func (s *scheduler) findGroup(ctx context.Context, user, name string) (feature.Group, string) {
	gs, err := s.h.Groups(ctx, user)
	if err != nil {
		logx.Warnf("listing groups failed: %v", err)
		return feature.Group{}, "Sorry, I couldn't look up your groups right now."
	}
	match := func(same func(g feature.Group) bool) []feature.Group {
		var out []feature.Group
		for _, g := range gs {
			if same(g) {
				out = append(out, g)
			}
		}
		return out
	}
	found := match(func(g feature.Group) bool { return strings.EqualFold(strings.TrimSpace(g.Name), name) })
	if p := plain(name); len(found) == 0 && p != "" {
		found = match(func(g feature.Group) bool { return plain(g.Name) == p })
	}
	switch {
	case len(found) == 1:
		return found[0], ""
	case len(found) > 1:
		return feature.Group{}, "More than one of your groups is called " + name + ": " + names(found) + ". Rename one, then try again."
	case len(gs) == 0:
		return feature.Group{}, "We aren't in any groups together."
	}
	return feature.Group{}, "We aren't in a group called " + name + " together. Your groups with me: " + names(gs) + "."
}

func names(gs []feature.Group) string {
	var out []string
	for _, g := range gs {
		out = append(out, g.Name)
	}
	return strings.Join(out, ", ")
}

// mine is the user's schedules, in order. s.mu is held.
func (s *scheduler) mine(user string) []*entry {
	var out []*entry
	for _, e := range s.all {
		if e.User == user {
			out = append(out, e)
		}
	}
	return out
}

// list shows the user's schedules, each with its ID and next run.
func (s *scheduler) list(ctx context.Context, user string) string {
	s.mu.Lock()
	mine := s.mine(user)
	s.mu.Unlock()
	if len(mine) == 0 {
		return "You have no schedules. /schedule help says how to make one."
	}
	var current map[string]string // the user's groups, by ID
	if slices.ContainsFunc(mine, func(e *entry) bool { return e.Group != "" }) {
		gs, err := s.h.Groups(ctx, user)
		if err != nil {
			logx.Warnf("listing groups failed: %v", err)
		} else {
			current = map[string]string{}
			for _, g := range gs {
				current[g.ID] = g.Name
			}
		}
	}
	now := s.now()
	var b strings.Builder
	b.WriteString("Your schedules:")
	for _, e := range mine {
		text := txt.Clamp(strings.Join(strings.Fields(e.Text), " "), 80)
		fmt.Fprintf(&b, "\n%d. \"%s\" %s", e.ID, text, e.first(now).Format(whenLayout))
		if e.Every != "" {
			b.WriteString(", every " + e.Every)
		}
		switch name, ok := current[e.Group]; {
		case e.Group == "":
			b.WriteString(", here")
		case ok || current == nil:
			b.WriteString(", in " + cmp.Or(name, e.GroupName))
		default:
			b.WriteString(", in " + e.GroupName + " (you've left it, so it won't post)")
		}
	}
	return b.String()
}

// del deletes the user's schedule with the number their list shows. The
// numbers never change or come back, so one that has run since the list
// can't name another.
func (s *scheduler) del(user, arg string) string {
	n, err := strconv.Atoi(arg)
	if err != nil {
		return "Say which one: /schedule del <number>, from /schedule list."
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := slices.IndexFunc(s.all, func(e *entry) bool { return e.User == user && e.ID == n })
	switch {
	case i < 0 && n >= 1 && n <= s.last[user]:
		return fmt.Sprintf("Schedule %d has already run or been deleted. /schedule list shows yours.", n)
	case i < 0:
		return fmt.Sprintf("You don't have a schedule %d. /schedule list shows yours.", n)
	}
	e := s.all[i]
	old := s.all
	s.all = slices.DeleteFunc(slices.Clone(old), func(x *entry) bool { return x == e })
	if err := s.save(); err != nil {
		s.all = old
		logx.Errorf("schedules not saved, so none was deleted: %v", err)
		return "Sorry, I couldn't delete that schedule: I can't write to my storage. Please let my owner know."
	}
	if e.cancel != nil {
		e.cancel()
	}
	return fmt.Sprintf("Deleted schedule %d: \"%s\".", n, txt.Clamp(strings.Join(strings.Fields(e.Text), " "), 80))
}

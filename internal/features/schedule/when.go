package schedule

import (
	"strings"
	"time"
)

// units are the repeats a schedule may have, with the longest an interval
// of each can be (a day is 25 hours when the clocks go back).
var units = map[string]time.Duration{
	"hour":    time.Hour,
	"day":     25 * time.Hour,
	"week":    7*24*time.Hour + time.Hour,
	"month":   31*24*time.Hour + time.Hour,
	"quarter": 92*24*time.Hour + time.Hour,
	"year":    366*24*time.Hour + time.Hour,
}

// months is how many months apart a calendar repeat's runs are.
var months = map[string]int{"month": 1, "quarter": 3, "year": 12}

// layouts are the ISO 8601 times accepted, without their offset.
var layouts = []string{"2006-01-02T15:04", "2006-01-02T15:04:05"}

// offsets are the ISO 8601 offsets accepted: Z, +10:00, +1000 or +10.
var offsets = []string{"Z07:00", "Z0700", "Z07"}

// savedLayout is how a time without an offset is saved.
const savedLayout = "2006-01-02T15:04:05"

// stamp is a schedule's start as the user wrote it: the date and wall clock
// time, kept apart from its zone so that no run inherits a time the clocks
// skipped.
type stamp struct {
	wall time.Time // the date and clock as written, held in UTC
	loc  *time.Location
}

// at is the instant s names.
func (s stamp) at() time.Time { return inZone(s.wall, s.loc) }

// inZone is the instant at which loc's clocks show the date and time of w,
// which is held in UTC. A time the clocks skip when they go forward is as
// far after the change as it was written after it: 02:30, on a night the
// clocks go from 02:00 to 03:00, is 03:30.
func inZone(w time.Time, loc *time.Location) time.Time {
	t := time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), w.Second(), w.Nanosecond(), loc)
	shown := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC)
	if gap := w.Sub(shown); gap > 0 { // Go read it with the offset from after the change
		t = t.Add(gap)
	}
	return t
}

// parseTime reads an ISO 8601 date and time, such as 2030-01-31T09:00 or
// 2030-01-31T09:00:00+10:00. One without an offset is in loc. It also
// returns the time written the way it is saved: without an offset if it had
// none, so it stays in the bot's time zone, else in RFC 3339.
func parseTime(s string, loc *time.Location) (stamp, string, bool) {
	s = strings.ToUpper(s) // "t" and "z" are allowed too
	for _, l := range layouts {
		// In UTC, which skips no times, so the clock stays as written.
		if w, err := time.Parse(l, s); err == nil {
			return checkYear(stamp{w, loc}, w.Format(savedLayout))
		}
		for _, o := range offsets {
			// In UTC, so that an offset that happens to match the bot's
			// zone still means a fixed offset, not the zone and its DST.
			if t, err := time.ParseInLocation(l+o, s, time.UTC); err == nil {
				w := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), 0, time.UTC)
				return checkYear(stamp{w, t.Location()}, t.Format(time.RFC3339))
			}
		}
	}
	return stamp{}, "", false
}

// checkYear refuses years far from now, which can only be typos, and keep
// next's search short.
func checkYear(s stamp, saved string) (stamp, string, bool) {
	if y := s.wall.Year(); y < 2000 || y > 2999 {
		return stamp{}, "", false
	}
	return s, saved, true
}

// occurrence is the kth run of a schedule from start, repeating every unit.
// A daily or longer repeat keeps start's wall clock time, worked out afresh
// for each run, so only a run on a night the clocks skip that time moves. A
// calendar repeat keeps start's day of the month where the month has it,
// else the month's last day: a monthly schedule from the 31st runs on the
// 30th in April, and a yearly one from 29 February on 28 February in other
// years.
func occurrence(start stamp, every string, k int) time.Time {
	w := start.wall
	switch every {
	case "hour":
		return start.at().Add(time.Duration(k) * time.Hour)
	case "day":
		return inZone(w.AddDate(0, 0, k), start.loc)
	case "week":
		return inZone(w.AddDate(0, 0, 7*k), start.loc)
	}
	y, m, d := w.Date()
	first := time.Date(y, m+time.Month(k*months[every]), 1, 0, 0, 0, 0, time.UTC)
	last := time.Date(first.Year(), first.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
	return inZone(time.Date(first.Year(), first.Month(), min(d, last), w.Hour(), w.Minute(), w.Second(), w.Nanosecond(), time.UTC), start.loc)
}

// next is a repeating schedule's first run after t, worked out from start
// so that a late run never shifts the ones after it.
func next(start stamp, every string, t time.Time) time.Time {
	k := 0
	if from := start.at(); t.After(from) {
		// One short of the runs that fit, since no interval is longer than
		// units says, but a first run the clocks moved may be an hour late.
		k = max(0, int(t.Sub(from)/units[every])-1)
	}
	for !occurrence(start, every, k).After(t) {
		k++
	}
	return occurrence(start, every, k)
}

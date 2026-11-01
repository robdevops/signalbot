package schedule

import (
	"strings"
	"testing"
	"time"
)

func date(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02T15:04", s, time.UTC)
	if err != nil {
		panic(err)
	}
	return t
}

// utc is a start written as s, in UTC.
func utc(s string) stamp { return stamp{date(s), time.UTC} }

func TestParseTime(t *testing.T) {
	plus10 := time.FixedZone("", 10*3600)
	for _, c := range []struct {
		in, saved string
		want      time.Time
	}{
		{"2030-01-31T09:00", "2030-01-31T09:00:00", date("2030-01-31T09:00")},
		{"2030-01-31t09:00:30", "2030-01-31T09:00:30", date("2030-01-31T09:00").Add(30 * time.Second)},
		{"2030-01-31T09:00Z", "2030-01-31T09:00:00Z", date("2030-01-31T09:00")},
		{"2030-01-31T09:00:00+10:00", "2030-01-31T09:00:00+10:00", time.Date(2030, 1, 31, 9, 0, 0, 0, plus10)},
		{"2030-01-31T09:00+1000", "2030-01-31T09:00:00+10:00", time.Date(2030, 1, 31, 9, 0, 0, 0, plus10)},
		{"2030-01-31T09:00-05", "2030-01-31T09:00:00-05:00", time.Date(2030, 1, 31, 14, 0, 0, 0, time.UTC)},
	} {
		got, saved, ok := parseTime(c.in, time.UTC)
		if !ok || !got.at().Equal(c.want) || saved != c.saved {
			t.Errorf("%s: got %v %q %v", c.in, got, saved, ok)
		}
	}
	for _, bad := range []string{"2030-01-31", "2030-02-30T09:00", "31/01/2030T09:00", "09:00", "1999-01-01T09:00", "tomorrow"} {
		if _, _, ok := parseTime(bad, time.UTC); ok {
			t.Errorf("%s was read", bad)
		}
	}
	// Without an offset, the time is in the bot's zone, and stays so when
	// it is read back.
	syd := time.FixedZone("AEST", 10*3600)
	got, saved, _ := parseTime("2030-01-31T09:00", syd)
	if got.at().Location() != syd || saved != "2030-01-31T09:00:00" {
		t.Errorf("got %v %q", got, saved)
	}
}

// Runs are worked out from the start: a monthly schedule from the 31st runs
// on each month's last day when it has no 31st, and is back on the 31st
// after; a yearly one from 29 February runs on 28 February in other years.
func TestOccurrence(t *testing.T) {
	check := func(start, every string, want ...string) {
		t.Helper()
		var got []string
		for k := range want {
			got = append(got, occurrence(utc(start), every, k).Format("2006-01-02T15:04"))
		}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s every %s: got %v", start, every, got)
		}
	}
	check("2030-01-31T09:00", "month", "2030-01-31T09:00", "2030-02-28T09:00", "2030-03-31T09:00", "2030-04-30T09:00")
	check("2028-02-29T08:00", "year", "2028-02-29T08:00", "2029-02-28T08:00", "2030-02-28T08:00", "2031-02-28T08:00", "2032-02-29T08:00")
	check("2030-11-30T10:00", "quarter", "2030-11-30T10:00", "2031-02-28T10:00", "2031-05-30T10:00", "2031-08-30T10:00")
	check("2030-12-30T23:00", "hour", "2030-12-30T23:00", "2030-12-31T00:00")
	check("2030-12-30T23:00", "day", "2030-12-30T23:00", "2030-12-31T23:00", "2031-01-01T23:00")
	check("2030-12-30T23:00", "week", "2030-12-30T23:00", "2031-01-06T23:00")
}

// A daily schedule keeps its wall clock time when the clocks change.
func TestOccurrenceAcrossDST(t *testing.T) {
	loc, err := time.LoadLocation("Europe/London")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	start, _, _ := parseTime("2030-03-30T09:00", loc) // the clocks go forward on the 31st
	if got := occurrence(start, "day", 1); got.Hour() != 9 || got.Sub(start.at()) != 23*time.Hour {
		t.Errorf("got %v", got)
	}
}

// A time the clocks skip when they go forward is saved as written. A
// one-off, or the run that night, is as far after the change as it was
// written after it, and every other run keeps the time asked for. Go reads
// such a time an hour early in some zones and an hour late in others; both
// come out the same.
func TestStartInTheDSTGap(t *testing.T) {
	for _, c := range []struct{ zone, dayBefore, gap, moved string }{
		{"America/New_York", "2030-03-09T02:30", "2030-03-10T02:30", "2030-03-10 03:30"}, // 02:00 to 03:00
		{"Europe/London", "2030-03-30T01:30", "2030-03-31T01:30", "2030-03-31 02:30"},    // 01:00 to 02:00
	} {
		loc, err := time.LoadLocation(c.zone)
		if err != nil {
			t.Skip("no tzdata:", err)
		}
		show := func(t time.Time) string { return t.In(loc).Format("2006-01-02 15:04") }
		s, saved, ok := parseTime(c.gap, loc)
		if !ok || saved != c.gap+":00" || show(s.at()) != c.moved {
			t.Errorf("%s: %s is %s, saved as %q", c.zone, c.gap, show(s.at()), saved)
		}
		if back, _, _ := parseTime(saved, loc); !back.at().Equal(s.at()) {
			t.Errorf("%s: read back as %s", c.zone, show(back.at()))
		}
		clock := c.gap[11:]
		start, _, _ := parseTime(c.dayBefore, loc)
		var got []string
		for k := range 4 {
			got = append(got, show(occurrence(start, "day", k)))
		}
		d0, d2, d3 := c.dayBefore[:10], date(c.dayBefore).AddDate(0, 0, 2), date(c.dayBefore).AddDate(0, 0, 3)
		want := []string{d0 + " " + clock, c.moved, d2.Format("2006-01-02") + " " + clock, d3.Format("2006-01-02") + " " + clock}
		if strings.Join(got, ", ") != strings.Join(want, ", ") {
			t.Errorf("%s: daily from %s: got %v, want %v", c.zone, c.dayBefore, got, want)
		}
		if n := show(next(start, "day", start.at().Add(time.Hour))); n != c.moved {
			t.Errorf("%s: next is %s", c.zone, n)
		}
		if n := show(next(s, "week", s.at())); n != date(c.gap).AddDate(0, 0, 7).Format("2006-01-02")+" "+clock {
			t.Errorf("%s: a week after a start in the gap is %s", c.zone, n)
		}
	}
}

// next is the first run after a time, from the start, however far behind.
func TestNext(t *testing.T) {
	for _, c := range []struct{ start, every, after, want string }{
		{"2030-01-31T09:00", "month", "2030-01-01T00:00", "2030-01-31T09:00"}, // the start is still to come
		{"2030-01-31T09:00", "month", "2030-01-31T09:00", "2030-02-28T09:00"}, // strictly after
		{"2030-01-31T09:00", "month", "2035-07-01T00:00", "2035-07-31T09:00"},
		{"2030-01-31T09:00", "month", "2035-06-30T10:00", "2035-07-31T09:00"},
		{"2028-02-29T08:00", "year", "2049-03-01T00:00", "2050-02-28T08:00"},
		{"2000-01-01T00:30", "hour", "2030-06-15T12:31", "2030-06-15T13:30"},
		{"2030-01-07T09:00", "week", "2030-03-01T00:00", "2030-03-04T09:00"},
		{"2030-01-01T09:00", "day", "2031-01-01T09:00", "2031-01-02T09:00"},
		{"2030-11-30T10:00", "quarter", "2031-03-01T00:00", "2031-05-30T10:00"},
	} {
		if got := next(utc(c.start), c.every, date(c.after)); !got.Equal(date(c.want)) {
			t.Errorf("%s every %s after %s: got %v, want %s", c.start, c.every, c.after, got, c.want)
		}
	}
}

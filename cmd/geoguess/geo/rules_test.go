package geo

import (
	"math"
	"slices"
	"testing"
	"time"
)

func TestSunElevation(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    Point
		t    time.Time
		want float64 // degrees, to within 0.5
	}{
		// At solar noon the sun is 90° − |latitude − declination| up, and at
		// midnight latitude + declination − 90°.
		{"June solstice noon at 40°N", Point{40, -105}, time.Date(2010, 6, 21, 19, 1, 40, 0, time.UTC), 73.44},
		{"March equinox noon at the equator", Point{0, 0}, time.Date(2024, 3, 20, 12, 7, 0, 0, time.UTC), 89.7},
		{"December solstice noon in Melbourne", Point{-37.81, 144.96}, time.Date(2025, 12, 21, 2, 19, 0, 0, time.UTC), 75.6},
		{"midnight in Melbourne", Point{-37.81, 144.96}, time.Date(2025, 12, 21, 14, 19, 0, 0, time.UTC), -28.7},
		{"midwinter noon in Tromsø", Point{69.65, 18.96}, time.Date(2025, 12, 21, 10, 45, 0, 0, time.UTC), -2.9},
		{"midsummer midnight in Tromsø", Point{69.65, 18.96}, time.Date(2025, 6, 21, 22, 45, 0, 0, time.UTC), 3.1},
	} {
		if got := SunElevation(tc.p, tc.t); math.Abs(got-tc.want) > 0.5 {
			t.Errorf("%s: %.2f°, want %.2f°", tc.name, got, tc.want)
		}
	}
}

func TestRulesCheck(t *testing.T) {
	now := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	noon := time.Date(2024, 12, 21, 2, 19, 0, 0, time.UTC) // in Melbourne
	mel := Point{-37.81, 144.96}
	good := Candidate{ID: "1", At: mel, Raw: mel, Captured: noon, Quality: 0.8, Width: 2048}
	rules := Rules{MinQuality: 0.5, MaxAge: 10 * 365 * 24 * time.Hour}
	for _, tc := range []struct {
		name string
		edit func(*Candidate)
		want error
	}{
		{"good", func(*Candidate) {}, nil},
		{"quality unknown", func(c *Candidate) { c.Quality = -1 }, nil},
		{"quality at the minimum", func(c *Candidate) { c.Quality = 0.5 }, nil},
		{"low quality", func(c *Candidate) { c.Quality = 0.49 }, RejectQuality},
		{"grade C", func(c *Candidate) { c.Quality, c.Grade = -1, 3 }, nil},
		{"grade D", func(c *Candidate) { c.Quality, c.Grade = -1, 2 }, RejectQuality},
		{"undated", func(c *Candidate) { c.Captured = time.Time{} }, RejectUndated},
		{"11 years old", func(c *Candidate) { c.Captured = now.AddDate(-11, 0, 0) }, RejectOld},
		{"width unknown", func(c *Candidate) { c.Width = 0 }, nil},
		{"narrow", func(c *Candidate) { c.Width = 1023 }, RejectSmall},
		{"fisheye", func(c *Candidate) { c.Fisheye = true }, RejectFisheye},
		{"night", func(c *Candidate) { c.Captured = noon.Add(12 * time.Hour) }, RejectDark},
		{"dusk", func(c *Candidate) { c.Captured = noon.Add(7 * time.Hour) }, RejectDark},
		{"morning", func(c *Candidate) { c.Captured = noon.Add(-4 * time.Hour) }, nil},
		{"40 m drift", func(c *Candidate) { c.Raw.Lat += 0.00036 }, nil},
		{"60 m drift", func(c *Candidate) { c.Raw.Lat += 0.00054 }, RejectDrift},
	} {
		c := good
		tc.edit(&c)
		if err := rules.Check(c, now); err != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestRulesFilter(t *testing.T) {
	now := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	noon := time.Date(2024, 12, 21, 2, 19, 0, 0, time.UTC)
	mel := Point{-37.81, 144.96}
	c := func(id, seq string, q float64) Candidate {
		return Candidate{ID: id, Source: "Mapillary", Sequence: seq, At: mel, Raw: mel, Captured: noon, Quality: q}
	}
	cands := []Candidate{
		c("1", "a", 0.1), // low quality: doesn't take its sequence
		c("2", "a", 0.9),
		c("3", "a", 0.9), // same sequence
		c("4", "b", 0.9), // used
		c("5", "b", 0.9),
		c("6", "", 0.9),
		c("7", "", 0.9), // no sequence: each is its own
	}
	var why []Reject
	used := func(key string) bool { return key == "Mapillary:4" }
	got := Rules{MinQuality: 0.5, MaxAge: 24 * 365 * time.Hour * 10}.Filter(cands, now, used, func(r Reject) { why = append(why, r) })
	var ids []string
	for _, g := range got {
		ids = append(ids, g.ID)
	}
	if want := []string{"2", "5", "6", "7"}; !slices.Equal(ids, want) {
		t.Errorf("kept %v, want %v", ids, want)
	}
	if want := []Reject{RejectQuality, RejectSequence, RejectUsed}; !slices.Equal(why, want) {
		t.Errorf("rejected for %v, want %v", why, want)
	}
	if got := (Rules{}).Filter(nil, now, nil, nil); len(got) != 0 {
		t.Errorf("Filter(nil) = %v", got)
	}
}

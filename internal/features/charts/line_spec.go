package charts

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/robdevops/signalbot/internal/kit/txt"
)

type linePoint struct {
	Date  string  `json:"date"`
	Value float64 `json:"value"`
	t     time.Time
}

type lineSeries struct {
	Name   string      `json:"name"`
	Axis   string      `json:"axis"` // "left" or "right"
	Points []linePoint `json:"points"`
}

// lineAxis is a Y axis: how its series are plotted and labelled, and, once
// laid out, its range and ticks.
type lineAxis struct {
	Title  string `json:"title"`
	Mode   string `json:"mode"` // "change" or "value"
	Unit   string `json:"unit"`
	Invert bool   `json:"invert"`

	used        bool // a series is on it
	lo, hi      float64
	top, bottom float64 // the plot's pixel rows
	ticks       []float64
	decimals    int
}

type lineSpec struct {
	Title    string               `json:"title"`
	Subtitle string               `json:"subtitle"`
	Axes     map[string]*lineAxis `json:"axes"`
	Series   []lineSeries         `json:"series"`

	left, right *lineAxis
}

func (s *lineSpec) axisOf(sr lineSeries) *lineAxis {
	if sr.Axis == "right" {
		return s.right
	}
	return s.left
}

// parseChart reads and checks the model's arguments. Errors are written for
// the model, so it can fix its call.
func (s *lineSpec) validate() error {
	s.Title = txt.Clamp(strings.TrimSpace(s.Title), 80)
	s.Subtitle = txt.Clamp(strings.TrimSpace(s.Subtitle), 120)
	if s.Title == "" {
		return errors.New("title is required")
	}
	for k := range s.Axes {
		if k != "left" && k != "right" {
			return fmt.Errorf(`axes may only have "left" and "right", not %q`, txt.Clamp(k, 20))
		}
	}
	s.left, s.right = cmp.Or(s.Axes["left"], &lineAxis{}), cmp.Or(s.Axes["right"], &lineAxis{})
	for _, a := range []*lineAxis{s.left, s.right} {
		a.Title, a.Unit = txt.Clamp(strings.TrimSpace(a.Title), 24), txt.Clamp(a.Unit, 4)
		switch a.Mode {
		case "":
			a.Mode = "change"
		case "change", "value":
		default:
			return errors.New(`an axis mode must be "change" or "value"`)
		}
	}
	if len(s.Series) < 1 || len(s.Series) > 6 {
		return errors.New("give 1 to 6 series")
	}
	for i := range s.Series {
		sr := &s.Series[i]
		sr.Name = txt.Clamp(strings.TrimSpace(sr.Name), 24)
		switch {
		case sr.Name == "":
			return fmt.Errorf("series %d has no name", i+1)
		case sr.Axis == "":
			sr.Axis = "left"
		case sr.Axis != "left" && sr.Axis != "right":
			return fmt.Errorf(`series %s: axis must be "left" or "right"`, sr.Name)
		}
		s.axisOf(*sr).used = true
	}
	if !s.left.used { // everything on the right: draw it as the only axis
		s.left, s.right = s.right, &lineAxis{Mode: "change"}
		for i := range s.Series {
			s.Series[i].Axis = "left"
		}
	}
	if s.right.used && (s.left.Title == "" || s.right.Title == "") {
		return errors.New("a chart that uses both axes needs a title on each: set axes.left.title and axes.right.title")
	}
	for i := range s.Series {
		sr := &s.Series[i]
		if len(sr.Points) < 2 || len(sr.Points) > 1000 {
			return fmt.Errorf("series %s needs 2 to 1000 points", sr.Name)
		}
		for j := range sr.Points {
			p := &sr.Points[j]
			t, err := time.Parse("2006-01-02", p.Date)
			if err != nil {
				return fmt.Errorf("series %s: date %q is not YYYY-MM-DD", sr.Name, txt.Clamp(p.Date, 20))
			}
			p.t = t
		}
		sort.SliceStable(sr.Points, func(a, b int) bool { return sr.Points[a].t.Before(sr.Points[b].t) })
		if s.axisOf(*sr).Mode == "change" {
			first := sr.Points[0].Value
			if first == 0 {
				return fmt.Errorf("series %s starts at 0, so %% change is undefined; use mode value for its axis", sr.Name)
			}
			for j := range sr.Points {
				sr.Points[j].Value = (sr.Points[j].Value - first) / first * 100
			}
		}
	}
	return nil
}

// format writes a value as the axis labels it.
func (a *lineAxis) format(v float64, decimals int) string {
	if a.Mode == "change" {
		if math.Abs(v) < 0.5*math.Pow(10, -float64(decimals)) {
			v = 0 // no "-0%"
		}
		return fmt.Sprintf("%.*f%%", decimals, v)
	}
	sign := ""
	if v < 0 {
		sign, v = "-", -v
	}
	return sign + a.Unit + fmt.Sprintf("%.*f", decimals, v)
}

// layout fits the axis to its series' values, plus 5% padding (and zero, in
// change mode), between pixel rows top and bottom; only the round ticks
// inside that range are kept.
func (a *lineAxis) layout(values []float64, top, bottom float64) {
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, v := range values {
		lo, hi = min(lo, v), max(hi, v)
	}
	if a.Mode == "change" {
		lo, hi = min(lo, 0), max(hi, 0)
	}
	pad := (hi - lo) * 0.05
	if pad == 0 {
		pad = math.Max(math.Abs(hi)*0.05, 1)
	}
	a.lo, a.hi, a.top, a.bottom = lo-pad, hi+pad, top, bottom
	all, decimals := niceTicks(a.lo, a.hi, 6)
	a.ticks, a.decimals = nil, decimals
	for _, v := range all {
		if v >= a.lo && v <= a.hi {
			a.ticks = append(a.ticks, v)
		}
	}
}

// y is the pixel row for v; an inverted axis puts higher values lower.
func (a *lineAxis) y(v float64) float64 {
	f := (v - a.lo) / (a.hi - a.lo)
	if a.Invert {
		return a.top + f*(a.bottom-a.top)
	}
	return a.bottom - f*(a.bottom-a.top)
}

// summary is what the model is told after the chart is drawn: the final
// value of each series, so its text can quote the same numbers.
// Summary is the chart in words: each series' final value.
func (s *lineSpec) summary() string {
	var parts []string
	for _, sr := range s.Series {
		parts = append(parts, sr.Name+" "+s.axisOf(sr).format(sr.Points[len(sr.Points)-1].Value, 1))
	}
	return "Final values: " + strings.Join(parts, ", ") + "."
}

// niceTicks returns about n evenly spaced round values (1, 2 or 5 × 10^k
// apart) covering lo..hi, and how many decimals their labels need.
func niceTicks(lo, hi float64, n int) (ticks []float64, decimals int) {
	if hi <= lo {
		hi = lo + 1
	}
	raw := (hi - lo) / float64(n)
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	step := mag * 10
	for _, m := range []float64{1, 2, 5} {
		if m*mag >= raw {
			step = m * mag
			break
		}
	}
	first, last := math.Floor(lo/step+1e-9), math.Ceil(hi/step-1e-9)
	for k := first; k <= last; k++ {
		ticks = append(ticks, k*step)
	}
	decimals = max(0, int(-math.Floor(math.Log10(step)+1e-9)))
	return ticks, decimals
}

// dateTick is an x-axis tick.
type dateTick struct {
	t     time.Time
	label string
}

// dateTicks picks tick dates for the span: days for up to 10 days, the 1st
// and 15th for up to two months, months up to two years, then years. They are
// thinned to at most 8.
func dateTicks(from, to time.Time) []dateTick {
	days := to.Sub(from).Hours() / 24
	var ticks []dateTick
	add := func(t time.Time, label string) {
		if !t.Before(from) && !t.After(to) {
			ticks = append(ticks, dateTick{t, label})
		}
	}
	switch {
	case days <= 10:
		for t := from; !t.After(to); t = t.AddDate(0, 0, 1) {
			add(t, t.Format("2 Jan"))
		}
	case days <= 62:
		for m := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.UTC); !m.After(to); m = m.AddDate(0, 1, 0) {
			add(m, m.Format("2 Jan"))
			add(m.AddDate(0, 0, 14), m.AddDate(0, 0, 14).Format("2 Jan"))
		}
	case days <= 731:
		for m := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.UTC); !m.After(to); m = m.AddDate(0, 1, 0) {
			label := m.Format("Jan")
			if m.Month() == time.January {
				label = m.Format("Jan 2006")
			}
			add(m, label)
		}
	default:
		for y := time.Date(from.Year(), 1, 1, 0, 0, 0, 0, time.UTC); !y.After(to); y = y.AddDate(1, 0, 0) {
			add(y, y.Format("2006"))
		}
	}
	if len(ticks) > 8 {
		step := (len(ticks) + 7) / 8
		var thin []dateTick
		for i := 0; i < len(ticks); i += step {
			thin = append(thin, ticks[i])
		}
		ticks = thin
	}
	return ticks
}

// spreadLabels moves label centres apart so no two are closer than gap,
// keeping them within lo..hi.
func spreadLabels(ys []float64, gap, lo, hi float64) []float64 {
	idx := make([]int, len(ys))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return ys[idx[a]] < ys[idx[b]] })
	out := append([]float64(nil), ys...)
	for k, i := range idx {
		out[i] = max(out[i], lo)
		if k > 0 {
			out[i] = max(out[i], out[idx[k-1]]+gap)
		}
	}
	for k := len(idx) - 1; k >= 0; k-- {
		i := idx[k]
		out[i] = min(out[i], hi)
		if k < len(idx)-1 {
			out[i] = min(out[i], out[idx[k+1]]-gap)
		}
	}
	return out
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

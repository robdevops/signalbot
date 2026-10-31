// Package linechart draws line charts of series over time, on one or two Y
// axes, as PNGs.
package linechart

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

type Point struct {
	Date  string  `json:"date"`
	Value float64 `json:"value"`
	t     time.Time
}

type Series struct {
	Name   string  `json:"name"`
	Axis   string  `json:"axis"` // "left" or "right"
	Points []Point `json:"points"`
}

// Axis is a Y axis: how its series are plotted and labelled, and, once
// laid out, its range and ticks.
type Axis struct {
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

type Spec struct {
	Title    string           `json:"title"`
	Subtitle string           `json:"subtitle"`
	Axes     map[string]*Axis `json:"axes"`
	Series   []Series         `json:"series"`

	left, right *Axis
}

func (s *Spec) axisOf(sr Series) *Axis {
	if sr.Axis == "right" {
		return s.right
	}
	return s.left
}

// parseChart reads and checks the model's arguments. Errors are written for
// the model, so it can fix its call.
func (s *Spec) Validate() error {
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
	s.left, s.right = cmp.Or(s.Axes["left"], &Axis{}), cmp.Or(s.Axes["right"], &Axis{})
	for _, a := range []*Axis{s.left, s.right} {
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
		s.left, s.right = s.right, &Axis{Mode: "change"}
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
func (a *Axis) format(v float64, decimals int) string {
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
func (a *Axis) layout(values []float64, top, bottom float64) {
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
func (a *Axis) y(v float64) float64 {
	f := (v - a.lo) / (a.hi - a.lo)
	if a.Invert {
		return a.top + f*(a.bottom-a.top)
	}
	return a.bottom - f*(a.bottom-a.top)
}

// summary is what the model is told after the chart is drawn: the final
// value of each series, so its text can quote the same numbers.
// Summary is the chart in words: each series' final value.
func (s *Spec) Summary() string {
	var parts []string
	for _, sr := range s.Series {
		parts = append(parts, sr.Name+" "+s.axisOf(sr).format(sr.Points[len(sr.Points)-1].Value, 1))
	}
	return "Final values: " + strings.Join(parts, ", ") + "."
}

package charts

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"

	"github.com/robdevops/signalbot/internal/kit/txt"
)

// chartTool is the tool the model calls to draw a line chart, which is
// sent to the chat as an image, on one Y axis; dualAxisTool draws one on two,
// offered only when a message asks for a second axis.
const (
	chartTool    = "make_chart"
	dualAxisTool = "make_dual_axis_chart"
)

// dualAxisToolSchema takes series on a left and a right Y axis;
// chartToolSchema is the same with the left axis alone.
var dualAxisToolSchema = json.RawMessage(`{
  "type": "object",
  "required": ["title", "series"],
  "properties": {
    "title": {"type": "string", "description": "Short title ending with the period in words, e.g. \"NVDA vs AMD, 1 month\" or \"Sydney rainfall, 5 years\"; it is also the message caption"},
    "subtitle": {"type": "string", "description": "e.g. \"% change over the past month, 30 Sep - 28 Oct 2026\""},
    "source": {"type": "string", "description": "` + sourceDescription + `"},
    "axes": {"type": "object", "description": "Settings for the left and right Y axes. Put series with a different unit or scale on the right axis. A chart that uses both axes must give each a title.",
      "properties": {"left": {"$ref": "#/$defs/axis"}, "right": {"$ref": "#/$defs/axis"}}},
    "series": {"type": "array", "minItems": 1, "maxItems": 6, "items": {
      "type": "object", "required": ["name", "points"],
      "properties": {
        "name": {"type": "string", "description": "Legend label, e.g. \"NVDA\" or \"Sydney\""},
        "axis": {"type": "string", "enum": ["left", "right"], "description": "Which Y axis the series uses (default left)"},
        "points": {"type": "array", "minItems": 2, "maxItems": 1000, "items": {
          "type": "object", "required": ["date", "value"],
          "properties": {"date": {"type": "string", "description": "YYYY-MM-DD"}, "value": {"type": "number"}}
        }}
      }
    }}
  },
  "$defs": {"axis": {"type": "object", "properties": {
    "title": {"type": "string", "description": "Axis title, e.g. \"Price ($)\" or \"Rainfall (mm)\"; required on both axes when both are used"},
    "mode": {"type": "string", "enum": ["value", "change"], "description": "value (default): plot the values as given, e.g. a population, a price or rainfall. change: plot each series as % change from its first point, to compare series on different scales, e.g. two stocks' performance"},
    "unit": {"type": "string", "description": "Prefix for values in value mode, e.g. \"$\""},
    "invert": {"type": "boolean", "description": "Draw this axis upside down: higher values lower"}
  }}}
}`)

var chartToolSchema = oneAxis(dualAxisToolSchema)

// oneAxis is a dual-axis chart schema with its axes replaced by one,
// "y_axis", and without a series' choice of axis: the fewer levels, the
// fewer mistakes a model makes.
func oneAxis(dual json.RawMessage) json.RawMessage {
	var s map[string]any
	if err := json.Unmarshal(dual, &s); err != nil {
		panic(err)
	}
	props := s["properties"].(map[string]any)
	delete(props, "axes")
	props["y_axis"] = map[string]any{"$ref": "#/$defs/axis"}
	item := props["series"].(map[string]any)["items"].(map[string]any)
	delete(item["properties"].(map[string]any), "axis")
	title := s["$defs"].(map[string]any)["axis"].(map[string]any)["properties"].(map[string]any)["title"].(map[string]any)
	title["description"] = "Axis title, e.g. \"Price ($)\" or \"Rainfall (mm)\""
	out, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return out
}

const (
	chartBody = "Use it when a chart would help, e.g. to show a trend or compare series. Pass the raw values: an axis shows them as given unless its mode is change, which computes the % change for you. " +
		"The chart is sent alone with its title as the caption, so write no other text: it would be discarded."
	chartToolDescription = "Draw a line chart of one or more series over time, on one Y axis, and attach it to your reply as an image. " +
		"To compare series on different scales, use change mode. " + chartBody
	dualAxisToolDescription = "Draw a line chart of one or more series over time on two Y axes, left and right, and attach it to your reply as an image. " +
		"Use it only because the user asked for a second axis; otherwise use " + chartTool + ". " + chartBody
)

type chartPoint struct {
	Date  string  `json:"date"`
	Value float64 `json:"value"`
	t     time.Time
}

type chartSeries struct {
	Name   string       `json:"name"`
	Axis   string       `json:"axis"` // "left" or "right"
	Points []chartPoint `json:"points"`
}

// chartAxis is a Y axis: how its series are plotted and labelled, and, once
// laid out, its range and ticks.
type chartAxis struct {
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

type chartSpec struct {
	Title    string                `json:"title"`
	Subtitle string                `json:"subtitle"`
	Source   string                `json:"source"`
	YAxis    *chartAxis            `json:"y_axis"` // make_chart's one axis: the left
	Axes     map[string]*chartAxis `json:"axes"`
	Series   []chartSeries         `json:"series"`

	left, right *chartAxis
}

// axisShorthand is arguments with an axis given as just its title taken as
// meant: "y_axis": "Population", or "axes": {"label": "Population"} (or
// "title" or "name") for the left axis. Anything else is left as it is.
func axisShorthand(arguments string) string {
	var tree any
	if json.Unmarshal([]byte(arguments), &tree) != nil {
		return arguments
	}
	if inner, ok := tree.(string); ok && json.Unmarshal([]byte(inner), &tree) != nil {
		return arguments
	}
	m, ok := tree.(map[string]any)
	if !ok {
		return arguments
	}
	changed := false
	if t, ok := m["y_axis"].(string); ok {
		m["y_axis"], changed = map[string]any{"title": t}, true
	}
	if axes, ok := m["axes"].(map[string]any); ok {
		for _, k := range []string{"label", "title", "name"} {
			if t, ok := axes[k].(string); ok {
				delete(axes, k)
				if _, set := axes["left"]; !set {
					axes["left"] = map[string]any{"title": t}
				}
				changed = true
			}
		}
	}
	if !changed {
		return arguments
	}
	out, err := json.Marshal(m)
	if err != nil {
		return arguments
	}
	return string(out)
}

func (s *chartSpec) axisOf(sr chartSeries) *chartAxis {
	if sr.Axis == "right" {
		return s.right
	}
	return s.left
}

// parseChart reads and checks the model's arguments. Errors are written for
// the model, so it can fix its call.
func parseChart(arguments string) (*chartSpec, error) {
	var s chartSpec
	if err := decodeArgs(chartTool, axisShorthand(arguments), &s); err != nil {
		return nil, err
	}
	if s.YAxis != nil {
		if s.Axes == nil {
			s.Axes = map[string]*chartAxis{}
		}
		if s.Axes["left"] == nil {
			s.Axes["left"] = s.YAxis
		}
	}
	s.Title = txt.Clamp(strings.TrimSpace(s.Title), 80)
	s.Subtitle = txt.Clamp(strings.TrimSpace(s.Subtitle), 120)
	s.Source = txt.Clamp(strings.TrimSpace(s.Source), 80)
	if s.Title == "" {
		return nil, errors.New("title is required")
	}
	for k := range s.Axes {
		if k != "left" && k != "right" {
			return nil, fmt.Errorf(`axes may only have "left" and "right", not %q`, txt.Clamp(k, 20))
		}
	}
	s.left, s.right = cmp.Or(s.Axes["left"], &chartAxis{}), cmp.Or(s.Axes["right"], &chartAxis{})
	for _, a := range []*chartAxis{s.left, s.right} {
		a.Title, a.Unit = txt.Clamp(strings.TrimSpace(a.Title), 24), txt.Clamp(a.Unit, 4)
		switch a.Mode {
		case "":
			a.Mode = "value"
		case "change", "value":
		default:
			return nil, errors.New(`an axis mode must be "change" or "value"`)
		}
	}
	if len(s.Series) < 1 || len(s.Series) > 6 {
		return nil, errors.New("give 1 to 6 series")
	}
	for i := range s.Series {
		sr := &s.Series[i]
		sr.Name = txt.Clamp(strings.TrimSpace(sr.Name), 24)
		switch {
		case sr.Name == "":
			return nil, fmt.Errorf("series %d has no name", i+1)
		case sr.Axis == "":
			sr.Axis = "left"
		case sr.Axis != "left" && sr.Axis != "right":
			return nil, fmt.Errorf(`series %s: axis must be "left" or "right"`, sr.Name)
		}
		s.axisOf(*sr).used = true
	}
	if !s.left.used { // everything on the right: draw it as the only axis
		s.left, s.right = s.right, &chartAxis{Mode: "value"}
		for i := range s.Series {
			s.Series[i].Axis = "left"
		}
	}
	if s.right.used && (s.left.Title == "" || s.right.Title == "") {
		return nil, errors.New("a chart that uses both axes needs a title on each: set axes.left.title and axes.right.title")
	}
	for i := range s.Series {
		sr := &s.Series[i]
		if len(sr.Points) < 2 || len(sr.Points) > 1000 {
			return nil, fmt.Errorf("series %s needs 2 to 1000 points", sr.Name)
		}
		for j := range sr.Points {
			p := &sr.Points[j]
			t, err := time.Parse("2006-01-02", p.Date)
			if err != nil {
				return nil, fmt.Errorf("series %s: date %q is not YYYY-MM-DD", sr.Name, txt.Clamp(p.Date, 20))
			}
			p.t = t
		}
		sort.SliceStable(sr.Points, func(a, b int) bool { return sr.Points[a].t.Before(sr.Points[b].t) })
		if s.axisOf(*sr).Mode == "change" {
			first := sr.Points[0].Value
			if first == 0 {
				return nil, fmt.Errorf("series %s starts at 0, so %% change is undefined; use mode value for its axis", sr.Name)
			}
			for j := range sr.Points {
				sr.Points[j].Value = (sr.Points[j].Value - first) / first * 100
			}
		}
	}
	return &s, nil
}

// format writes a value as the axis labels it.
func (a *chartAxis) format(v float64, decimals int) string {
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
	return sign + a.Unit + commas(fmt.Sprintf("%.*f", decimals, v))
}

// layout fits the axis to its series' values, plus 5% padding (and zero, in
// change mode), between pixel rows top and bottom; only the round ticks
// inside that range are kept.
func (a *chartAxis) layout(values []float64, top, bottom float64) {
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
func (a *chartAxis) y(v float64) float64 {
	f := (v - a.lo) / (a.hi - a.lo)
	if a.Invert {
		return a.top + f*(a.bottom-a.top)
	}
	return a.bottom - f*(a.bottom-a.top)
}

// summary is what the model is told after the chart is drawn: the final
// value of each series, so its text can quote the same numbers.
func (s *chartSpec) summary() string {
	var parts []string
	for _, sr := range s.Series {
		parts = append(parts, sr.Name+" "+s.axisOf(sr).format(sr.Points[len(sr.Points)-1].Value, 1))
	}
	return chartNote + "Final values: " + strings.Join(parts, ", ") + "."
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
	for k, i := range slices.Backward(idx) {
		out[i] = min(out[i], hi)
		if k < len(idx)-1 {
			out[i] = min(out[i], out[idx[k+1]]-gap)
		}
	}
	return out
}

// theme is the colours a chart is drawn in.
type theme struct {
	bg, title, muted, grid, zero color.RGBA
	up, down                     color.RGBA // a single series that rose or fell
	palette                      []color.RGBA
}

// lightTheme is dark text on white.
var lightTheme = &theme{
	bg:    color.RGBA{0xff, 0xff, 0xff, 0xff},
	title: color.RGBA{0x11, 0x18, 0x27, 0xff},
	muted: color.RGBA{0x6b, 0x72, 0x80, 0xff},
	grid:  color.RGBA{0xe5, 0xe7, 0xeb, 0xff},
	zero:  color.RGBA{0x9c, 0xa3, 0xaf, 0xff},
	up:    color.RGBA{0x16, 0xa3, 0x4a, 0xff},
	down:  color.RGBA{0xdc, 0x26, 0x26, 0xff},
	// The palette serves both chart types; line charts use the first 6.
	palette: []color.RGBA{
		{0x25, 0x63, 0xeb, 0xff}, // blue
		{0xf9, 0x73, 0x16, 0xff}, // orange
		{0x16, 0xa3, 0x4a, 0xff}, // green
		{0xdc, 0x26, 0x26, 0xff}, // red
		{0x93, 0x33, 0xea, 0xff}, // purple
		{0x08, 0x91, 0xb2, 0xff}, // teal
		{0xca, 0x8a, 0x04, 0xff}, // amber
		{0xdb, 0x27, 0x77, 0xff}, // pink
		{0x4f, 0x46, 0xe5, 0xff}, // indigo
		{0x65, 0xa3, 0x0d, 0xff}, // lime
		{0x47, 0x55, 0x69, 0xff}, // slate
		{0x92, 0x40, 0x0e, 0xff}, // brown
	},
}

// darkTheme is light text on near-black navy, with the palette's colours in
// the same order, lighter so they stand out.
var darkTheme = &theme{
	bg:    color.RGBA{0x14, 0x18, 0x20, 0xff},
	title: color.RGBA{0xf1, 0xf3, 0xf7, 0xff},
	muted: color.RGBA{0x9c, 0xa3, 0xaf, 0xff},
	grid:  color.RGBA{0x2a, 0x30, 0x3c, 0xff},
	zero:  color.RGBA{0x6b, 0x72, 0x80, 0xff},
	up:    color.RGBA{0x34, 0xc7, 0x6f, 0xff},
	down:  color.RGBA{0xf4, 0x3f, 0x5e, 0xff},
	palette: []color.RGBA{
		{0x3b, 0x8b, 0xe8, 0xff}, // blue
		{0xe0, 0x62, 0x2d, 0xff}, // orange
		{0x34, 0xc7, 0x6f, 0xff}, // green
		{0xf4, 0x3f, 0x5e, 0xff}, // red, toward crimson to stay clear of the orange
		{0xa8, 0x6e, 0xf5, 0xff}, // purple
		{0x22, 0xb8, 0xd4, 0xff}, // teal
		{0xe8, 0xb0, 0x20, 0xff}, // amber
		{0xf0, 0x7a, 0xc8, 0xff}, // pink, toward magenta to stay clear of the red
		{0x81, 0x7c, 0xf5, 0xff}, // indigo
		{0x8c, 0xc8, 0x3a, 0xff}, // lime
		{0x94, 0xa3, 0xb8, 0xff}, // slate
		{0xa3, 0x78, 0x5f, 0xff}, // brown, duller to stay clear of the orange
	},
}

// chartFonts are the bundled Go fonts, parsed once.
var chartFonts = func() (f struct{ bold, regular *opentype.Font }) {
	var err error
	if f.bold, err = opentype.Parse(gobold.TTF); err != nil {
		panic(err)
	}
	if f.regular, err = opentype.Parse(goregular.TTF); err != nil {
		panic(err)
	}
	return f
}()

func face(f *opentype.Font, size float64) font.Face {
	fc, err := opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		panic(err) // only for a bad size, which these constants are not
	}
	return fc
}

// canvas draws anti-aliased shapes and text on an image, in its theme.
type canvas struct {
	img *image.RGBA
	th  *theme
}

// fill paints polygons, each given as x,y pairs, in c.
func (cv *canvas) fill(c color.Color, polys ...[]float64) {
	b := cv.img.Bounds()
	r := vector.NewRasterizer(b.Dx(), b.Dy())
	for _, p := range polys {
		// Every polygon is wound the same way, so overlaps add up instead of
		// cancelling out.
		area := 0.0
		for i := 0; i < len(p); i += 2 {
			j := (i + 2) % len(p)
			area += p[i]*p[j+1] - p[j]*p[i+1]
		}
		if area < 0 {
			q := make([]float64, len(p))
			for i := 0; i < len(p); i += 2 {
				q[len(p)-2-i], q[len(p)-1-i] = p[i], p[i+1]
			}
			p = q
		}
		r.MoveTo(float32(p[0]), float32(p[1]))
		for i := 2; i < len(p); i += 2 {
			r.LineTo(float32(p[i]), float32(p[i+1]))
		}
		r.ClosePath()
	}
	r.Draw(cv.img, b, image.NewUniform(c), image.Point{})
}

func circle(x, y, rad float64) []float64 {
	var p []float64
	for i := range 16 {
		a := float64(i) * math.Pi / 8
		p = append(p, x+rad*math.Cos(a), y+rad*math.Sin(a))
	}
	return p
}

// line draws a polyline of the given width with round joins and ends.
func (cv *canvas) line(c color.RGBA, width float64, pts ...float64) {
	w := width / 2
	var polys [][]float64
	for i := 0; i+3 < len(pts); i += 2 {
		x1, y1, x2, y2 := pts[i], pts[i+1], pts[i+2], pts[i+3]
		dx, dy := x2-x1, y2-y1
		l := math.Hypot(dx, dy)
		if l == 0 {
			continue
		}
		nx, ny := -dy/l*w, dx/l*w
		polys = append(polys, []float64{x1 + nx, y1 + ny, x2 + nx, y2 + ny, x2 - nx, y2 - ny, x1 - nx, y1 - ny})
	}
	if width > 1.5 {
		for i := 0; i+1 < len(pts); i += 2 {
			polys = append(polys, circle(pts[i], pts[i+1], w))
		}
	}
	if len(polys) > 0 {
		cv.fill(c, polys...)
	}
}

func (cv *canvas) rect(c color.RGBA, x, y, w, h float64) { cv.fill(c, rectPoly(x, y, w, h)) }

// text draws s with its baseline at y; align is 0 for left, 0.5 for centre
// and 1 for right of x.
func (cv *canvas) text(f font.Face, c color.RGBA, x, y float64, align float64, s string) {
	d := font.Drawer{Dst: cv.img, Src: image.NewUniform(c), Face: f}
	w := float64(d.MeasureString(s)) / 64
	d.Dot = fixed.Point26_6{X: fixed.Int26_6((x - w*align) * 64), Y: fixed.Int26_6(y * 64)}
	d.DrawString(s)
}

// width and height are the size of every chart, in pixels.
const width, height = 1280, 800

// sourceBaseline is the row a chart's source is written on, and
// sourceTop where its letters begin: nothing else is drawn below that.
const sourceBaseline, sourceTop = height - 12, height - 26

// sourceDescription is the schema's description of both tools' "source".
const sourceDescription = "Where the data came from, e.g. the site or tool; fill it in when the data came from a tool or search"

// newCanvas is a blank chart in theme th with its title and subtitle drawn,
// and its source, if any, small at the bottom left, below the date labels
// and the legend.
func newCanvas(th *theme, title, subtitle, source string) *canvas {
	cv := &canvas{img: image.NewRGBA(image.Rect(0, 0, width, height)), th: th}
	cv.rect(th.bg, 0, 0, width, height)
	titleFace, subFace := face(chartFonts.bold, 34), face(chartFonts.regular, 22)
	defer titleFace.Close()
	defer subFace.Close()
	cv.text(titleFace, th.title, 38, 46, 0, title)
	if subtitle != "" {
		cv.text(subFace, th.muted, 38, 80, 0, subtitle)
	}
	if source != "" {
		f := face(chartFonts.regular, 16)
		defer f.Close()
		cv.text(f, th.muted, 38, sourceBaseline, 0, fitText(f, "Source: "+source, width-2*38))
	}
	return cv
}

// fitText is s, cut with "…" until it is no wider than w in face f.
func fitText(f font.Face, s string, w float64) string {
	for textWidth(f, s) > w && len([]rune(s)) > 1 {
		r := []rune(strings.TrimSuffix(s, "…"))
		s = string(r[:len(r)-1]) + "…"
	}
	return s
}

// encode is the chart as a PNG of at most 256 colours, about a third the
// size of full colour with no visible change. Colours that differ only in
// their low bits, the in-between shades of anti-aliased edges, are grouped,
// by as few bits as fit 256 groups (at most 4 of 8), and each group is drawn
// in its most common colour, so the theme's colours stay exact. Groups past
// 256, if any, take the nearest colour kept.
func (cv *canvas) encode() ([]byte, error) {
	pix := cv.img.Pix
	rgb := func(i int) uint32 { return uint32(pix[i])<<16 | uint32(pix[i+1])<<8 | uint32(pix[i+2]) }
	counts := map[uint32]int{} // pixels of each colour
	for i := 0; i < len(pix); i += 4 {
		counts[rgb(i)]++
	}
	var mask uint32
	best, size := map[uint32]uint32{}, map[uint32]int{} // each group's most common colour, and its pixels
	for bits := 0; bits <= 4; bits++ {
		mask = uint32(0xff<<bits&0xff) * 0x010101
		clear(best)
		clear(size)
		for c, n := range counts {
			g := c & mask
			if b, ok := best[g]; !ok || n > counts[b] || n == counts[b] && c < b {
				best[g] = c
			}
			size[g] += n
		}
		if len(best) <= 256 {
			break
		}
	}
	groups := make([]uint32, 0, len(best))
	for g := range best {
		groups = append(groups, g)
	}
	slices.SortFunc(groups, func(a, b uint32) int { return cmp.Or(size[b]-size[a], cmp.Compare(a, b)) })
	groups = groups[:min(256, len(groups))]
	pal := make(color.Palette, len(groups))
	index := map[uint32]uint8{} // of each group kept
	for i, g := range groups {
		c := best[g]
		pal[i], index[g] = color.RGBA{uint8(c >> 16), uint8(c >> 8), uint8(c), 0xff}, uint8(i)
	}
	for c := range counts { // counts now holds each colour's index
		i, ok := index[c&mask]
		if !ok {
			i = uint8(pal.Index(color.RGBA{uint8(c >> 16), uint8(c >> 8), uint8(c), 0xff}))
		}
		counts[c] = int(i)
	}
	out := image.NewPaletted(cv.img.Rect, pal)
	for i := range out.Pix {
		out.Pix[i] = uint8(counts[rgb(4*i)])
	}
	var buf bytes.Buffer
	err := png.Encode(&buf, out)
	return buf.Bytes(), err
}

// renderChart draws the chart in theme th as a PNG.
func renderChart(s *chartSpec, th *theme) ([]byte, error) {
	return drawChart(s, th).encode()
}

// drawChart draws the chart in theme th.
func drawChart(s *chartSpec, th *theme) *canvas {
	const left, top, bottom = 100.0, 125.0, 700.0
	two, single := s.right.used, len(s.Series) == 1
	right, labelX := 1000.0, 1030.0 // plot edge, end labels
	switch {
	case two:
		right, labelX = 920.0, 1010.0 // room for the right axis's labels
	case single:
		right = 1140 // room for the current value beside the end dot
	}
	cv := newCanvas(th, s.Title, s.Subtitle, s.Source)
	axisFace, axisTitleFace := face(chartFonts.regular, 24), face(chartFonts.regular, 18)
	defer axisFace.Close()
	defer axisTitleFace.Close()

	from, to := s.Series[0].Points[0].t, s.Series[0].Points[0].t
	values := map[*chartAxis][]float64{}
	for _, sr := range s.Series {
		a := s.axisOf(sr)
		for _, p := range sr.Points {
			from, to = minTime(from, p.t), maxTime(to, p.t)
			values[a] = append(values[a], p.Value)
		}
	}
	s.left.layout(values[s.left], top, bottom)
	if two {
		s.right.layout(values[s.right], top, bottom)
	}
	span := to.Sub(from).Seconds()
	if span == 0 {
		span = 1
	}
	px := func(t time.Time) float64 { return left + t.Sub(from).Seconds()/span*(right-left) }

	// Gridlines and labels from the left axis; labels only from the right.
	for _, v := range s.left.ticks {
		y := s.left.y(v)
		c := th.grid
		if s.left.Mode == "change" && v == 0 {
			c = th.zero
		}
		cv.line(c, 1.5, left, y, right+10, y)
		cv.text(axisFace, th.muted, left-12, y+8, 1, s.left.format(v, s.left.decimals))
	}
	if two {
		if s.right.Mode == "change" && s.left.Mode != "change" {
			y := s.right.y(0)
			cv.line(th.zero, 1.5, left, y, right+10, y)
		}
		for _, v := range s.right.ticks {
			cv.text(axisFace, th.muted, right+14, s.right.y(v)+8, 0, s.right.format(v, s.right.decimals))
		}
	}
	if s.left.Title != "" {
		cv.text(axisTitleFace, th.muted, 38, top-14, 0, s.left.Title)
	}
	if two {
		cv.text(axisTitleFace, th.muted, labelX-10, top-14, 1, s.right.Title)
	}
	for _, tk := range dateTicks(from, to) {
		x := px(tk.t)
		cv.line(th.muted, 1.5, x, bottom+14, x, bottom+24)
		cv.text(axisFace, th.muted, x, bottom+60, 0.5, tk.label)
	}

	if single {
		drawArea(cv, s, px, bottom, left, right)
	} else {
		drawLines(cv, s, px, top, bottom, right, labelX, two)
	}
	return cv
}

// drawLines draws two or more series, each with an end label.
func drawLines(cv *canvas, s *chartSpec, px func(time.Time) float64, top, bottom, right, labelX float64, two bool) {
	th, labelFace := cv.th, face(chartFonts.regular, 20)
	defer labelFace.Close()
	ends := make([]float64, len(s.Series))
	for i, sr := range s.Series {
		a := s.axisOf(sr)
		pts := make([]float64, 0, 2*len(sr.Points))
		for _, p := range sr.Points {
			pts = append(pts, px(p.t), a.y(p.Value))
		}
		cv.line(th.palette[i], 3, pts...)
		ends[i] = a.y(sr.Points[len(sr.Points)-1].Value)
	}
	labels := spreadLabels(ends, 28, top, bottom)
	for i, sr := range s.Series {
		c := th.palette[i]
		lastX := px(sr.Points[len(sr.Points)-1].t)
		if two {
			cv.line(c, 1.5, lastX, ends[i], right+8, ends[i]) // the right axis's labels sit beyond
		} else {
			cv.line(c, 1.5, lastX, ends[i], right+20, ends[i], labelX, labels[i])
		}
		cv.rect(c, labelX, labels[i]-6, 12, 12)
		cv.text(labelFace, th.title, labelX+18, labels[i]+7, 0,
			sr.Name+"  "+s.axisOf(sr).format(sr.Points[len(sr.Points)-1].Value, 1))
	}
}

// drawArea draws a single series as an area chart: green if it rose over the
// period, red if it fell, with a translucent fill down to the bottom of the
// plot, its high and low marked and a dot on its last point. It is styling
// only; the model isn't told.
func drawArea(cv *canvas, s *chartSpec, px func(time.Time) float64, bottom, left, right float64) {
	a, pts, th := s.left, s.Series[0].Points, cv.th
	c := th.up
	if pts[len(pts)-1].Value < pts[0].Value {
		c = th.down
	}
	decimals := 2
	if a.Mode == "change" {
		decimals = 1
	}
	line := make([]float64, 0, 2*len(pts))
	for _, p := range pts {
		line = append(line, px(p.t), a.y(p.Value))
	}
	area := append(append([]float64(nil), line...), px(pts[len(pts)-1].t), bottom, px(pts[0].t), bottom)
	cv.fill(color.NRGBA{c.R, c.G, c.B, 0x26}, area) // about 15% opacity
	cv.line(c, 3, line...)

	hi, lo := highLow(pts)
	markFace := face(chartFonts.regular, 18)
	defer markFace.Close()
	for _, m := range []struct {
		i     int
		word  string
		above bool
	}{{hi, "High", !a.Invert}, {lo, "Low", a.Invert}} {
		p := pts[m.i]
		x, y := px(p.t), a.y(p.Value)
		lines := [2]string{m.word + " " + a.format(p.Value, decimals), p.t.Format("2 Jan 2006")}
		cv.fill(th.title, circle(x, y, 6.5))
		cv.fill(th.bg, circle(x, y, 4.5))
		// Above the high and below the low (swapped on an inverted axis),
		// centred on the point unless that would run off the side. Where that
		// would cross the subtitle or the date labels, it goes beside the point.
		align, tx := 0.5, x
		switch {
		case x < left+110:
			align, tx = 0, x-10
		case x > right-110:
			align, tx = 1, x+10
		}
		base := y + 30 // first line's baseline, below the point
		if m.above {
			base = y - 36
		}
		if base-18 < 90 || base+22 > bottom+8 {
			base, align, tx = y-4, 1, x-14 // to the left of the point
			if x < left+260 {
				align, tx = 0, x+14 // to the right, near the left edge
			}
		}
		// A translucent backing in the background colour keeps the label
		// readable where it crosses the line.
		w := max(textWidth(markFace, lines[0]), textWidth(markFace, lines[1]))
		cv.fill(color.NRGBA{th.bg.R, th.bg.G, th.bg.B, 0xd9}, rectPoly(tx-w*align-4, base-17, w+8, 45))
		cv.text(markFace, th.title, tx, base, align, lines[0])
		cv.text(markFace, th.title, tx, base+22, align, lines[1])
	}
	// The current value beside the end dot, in the line's colour.
	last := pts[len(pts)-1]
	x, y := px(last.t), a.y(last.Value)
	cv.fill(th.bg, circle(x, y, 7.5))
	cv.fill(c, circle(x, y, 5.5))
	valueFace := face(chartFonts.bold, 20)
	defer valueFace.Close()
	y = min(max(y, 100), bottom-14) // keep both lines inside the image
	cv.text(valueFace, c, x+14, y-2, 0, a.format(last.Value, decimals))
	cv.text(markFace, th.muted, x+14, y+20, 0, last.t.Format("2 Jan 2006"))
}

// textWidth is how wide s is in pixels in face f.
func textWidth(f font.Face, s string) float64 {
	return float64(font.MeasureString(f, s)) / 64
}

func rectPoly(x, y, w, h float64) []float64 {
	return []float64{x, y, x + w, y, x + w, y + h, x, y + h}
}

// highLow returns the indices of the first highest and first lowest points.
func highLow(pts []chartPoint) (hi, lo int) {
	for i, p := range pts {
		if p.Value > pts[hi].Value {
			hi = i
		}
		if p.Value < pts[lo].Value {
			lo = i
		}
	}
	return hi, lo
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

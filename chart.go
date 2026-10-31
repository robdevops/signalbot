package main

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

const (
	// maxReplyImages caps the images one reply may carry.
	maxReplyImages = 4
	// maxImageBytes caps one image, sent or received.
	maxImageBytes = 5 << 20
)

// sendableImage reports whether data is an image Signal can show, small
// enough to send.
func sendableImage(data []byte) bool {
	switch http.DetectContentType(data) {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return len(data) <= maxImageBytes
	}
	return false
}

// imageDataURI is data as a signal-cli attachment.
func imageDataURI(data []byte, name string) string {
	mime := http.DetectContentType(data)
	ext := strings.TrimPrefix(mime, "image/")
	return "data:" + mime + ";filename=" + name + "." + ext + ";base64," + base64.StdEncoding.EncodeToString(data)
}

// chartTool is the built-in tool the model calls to draw a line chart, which
// is sent to the chat as an image.
const chartTool = "make_chart"

var chartToolSchema = json.RawMessage(`{
  "type": "object",
  "required": ["title", "series"],
  "properties": {
    "title": {"type": "string", "description": "Short title, e.g. \"NVDA vs ACMR\""},
    "subtitle": {"type": "string", "description": "e.g. \"% change over the past month, 30 Sep - 28 Oct 2026\""},
    "axes": {"type": "object", "description": "Settings for the left and right Y axes. Put series with a different unit or scale on the right axis. A chart that uses both axes must give each a title.",
      "properties": {"left": {"$ref": "#/$defs/axis"}, "right": {"$ref": "#/$defs/axis"}}},
    "series": {"type": "array", "minItems": 1, "maxItems": 6, "items": {
      "type": "object", "required": ["name", "points"],
      "properties": {
        "name": {"type": "string", "description": "e.g. a ticker"},
        "axis": {"type": "string", "enum": ["left", "right"], "description": "Which Y axis the series uses (default left)"},
        "points": {"type": "array", "minItems": 2, "maxItems": 1000, "items": {
          "type": "object", "required": ["date", "value"],
          "properties": {"date": {"type": "string", "description": "YYYY-MM-DD"}, "value": {"type": "number"}}
        }}
      }
    }}
  },
  "$defs": {"axis": {"type": "object", "properties": {
    "title": {"type": "string", "description": "Axis title, e.g. \"Price ($)\"; required on both axes when both are used"},
    "mode": {"type": "string", "enum": ["change", "value"], "description": "change (default): plot each series as % change from its first point. value: plot the values as given"},
    "unit": {"type": "string", "description": "Prefix for values in value mode, e.g. \"$\""},
    "invert": {"type": "boolean", "description": "Draw this axis upside down: higher values lower"}
  }}}
}`)

const chartToolDescription = "Draw a line chart of one or more series over time, on one or two Y axes, and attach it to your reply as an image. " +
	"Use it when a chart would help, e.g. to compare prices. Pass the raw values: in change mode the % change is computed for you."

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
	Axes     map[string]*chartAxis `json:"axes"`
	Series   []chartSeries         `json:"series"`

	left, right *chartAxis
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
	if err := json.Unmarshal([]byte(arguments), &s); err != nil {
		return nil, errors.New("the arguments are not valid JSON for make_chart")
	}
	s.Title = clamp(strings.TrimSpace(s.Title), 80)
	s.Subtitle = clamp(strings.TrimSpace(s.Subtitle), 120)
	if s.Title == "" {
		return nil, errors.New("title is required")
	}
	for k := range s.Axes {
		if k != "left" && k != "right" {
			return nil, fmt.Errorf(`axes may only have "left" and "right", not %q`, clamp(k, 20))
		}
	}
	s.left, s.right = cmp.Or(s.Axes["left"], &chartAxis{}), cmp.Or(s.Axes["right"], &chartAxis{})
	for _, a := range []*chartAxis{s.left, s.right} {
		a.Title, a.Unit = clamp(strings.TrimSpace(a.Title), 24), clamp(a.Unit, 4)
		switch a.Mode {
		case "":
			a.Mode = "change"
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
		sr.Name = clamp(strings.TrimSpace(sr.Name), 24)
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
		s.left, s.right = s.right, &chartAxis{Mode: "change"}
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
				return nil, fmt.Errorf("series %s: date %q is not YYYY-MM-DD", sr.Name, clamp(p.Date, 20))
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
	return sign + a.Unit + fmt.Sprintf("%.*f", decimals, v)
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
	return "Chart attached to your reply. Final values: " + strings.Join(parts, ", ") + "."
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

var (
	chartBG      = color.RGBA{0x14, 0x18, 0x1f, 0xff}
	chartTitle   = color.RGBA{0xf2, 0xf4, 0xf8, 0xff}
	chartMuted   = color.RGBA{0x9a, 0xa3, 0xad, 0xff}
	chartGrid    = color.RGBA{0x2a, 0x30, 0x3a, 0xff}
	chartZero    = color.RGBA{0x6b, 0x72, 0x80, 0xff}
	chartPalette = []color.RGBA{
		{0xe0, 0x5a, 0x2b, 0xff}, // orange
		{0x3b, 0x8b, 0xea, 0xff}, // blue
		{0x3f, 0xb9, 0x50, 0xff}, // green
		{0xa3, 0x71, 0xf7, 0xff}, // purple
		{0xe3, 0xb3, 0x41, 0xff}, // yellow
		{0xe8, 0x5a, 0xad, 0xff}, // pink
	}
)

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

// canvas draws anti-aliased shapes and text on an image.
type canvas struct {
	img *image.RGBA
}

// fill paints polygons, each given as x,y pairs, in c.
func (cv *canvas) fill(c color.RGBA, polys ...[]float64) {
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
	for i := 0; i < 16; i++ {
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

func (cv *canvas) rect(c color.RGBA, x, y, w, h float64) {
	cv.fill(c, []float64{x, y, x + w, y, x + w, y + h, x, y + h})
}

// text draws s with its baseline at y; align is 0 for left, 0.5 for centre
// and 1 for right of x.
func (cv *canvas) text(f font.Face, c color.RGBA, x, y float64, align float64, s string) {
	d := font.Drawer{Dst: cv.img, Src: image.NewUniform(c), Face: f}
	w := float64(d.MeasureString(s)) / 64
	d.Dot = fixed.Point26_6{X: fixed.Int26_6((x - w*align) * 64), Y: fixed.Int26_6(y * 64)}
	d.DrawString(s)
}

// renderChart draws the chart as a 1280×800 PNG.
func renderChart(s *chartSpec) ([]byte, error) {
	const W, H = 1280, 800
	const left, top, bottom = 100.0, 125.0, 700.0
	two := s.right.used
	right, labelX := 1000.0, 1030.0 // plot edge, end labels
	if two {
		right, labelX = 920.0, 1010.0 // room for the right axis's labels
	}
	cv := &canvas{img: image.NewRGBA(image.Rect(0, 0, W, H))}
	cv.rect(chartBG, 0, 0, W, H)

	titleFace, subFace := face(chartFonts.bold, 34), face(chartFonts.regular, 22)
	axisFace, labelFace, axisTitleFace := face(chartFonts.regular, 24), face(chartFonts.regular, 20), face(chartFonts.regular, 18)
	for _, f := range []font.Face{titleFace, subFace, axisFace, labelFace, axisTitleFace} {
		defer f.Close()
	}
	cv.text(titleFace, chartTitle, 38, 46, 0, s.Title)
	if s.Subtitle != "" {
		cv.text(subFace, chartMuted, 38, 80, 0, s.Subtitle)
	}

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
		c := chartGrid
		if s.left.Mode == "change" && v == 0 {
			c = chartZero
		}
		cv.line(c, 1.5, left, y, right+10, y)
		cv.text(axisFace, chartMuted, left-12, y+8, 1, s.left.format(v, s.left.decimals))
	}
	if two {
		if s.right.Mode == "change" && s.left.Mode != "change" {
			y := s.right.y(0)
			cv.line(chartZero, 1.5, left, y, right+10, y)
		}
		for _, v := range s.right.ticks {
			cv.text(axisFace, chartMuted, right+14, s.right.y(v)+8, 0, s.right.format(v, s.right.decimals))
		}
	}
	if s.left.Title != "" {
		cv.text(axisTitleFace, chartMuted, 38, top-14, 0, s.left.Title)
	}
	if two {
		cv.text(axisTitleFace, chartMuted, labelX-10, top-14, 1, s.right.Title)
	}
	for _, tk := range dateTicks(from, to) {
		x := px(tk.t)
		cv.line(chartMuted, 1.5, x, bottom+14, x, bottom+24)
		cv.text(axisFace, chartMuted, x, bottom+60, 0.5, tk.label)
	}

	ends := make([]float64, len(s.Series))
	for i, sr := range s.Series {
		a := s.axisOf(sr)
		pts := make([]float64, 0, 2*len(sr.Points))
		for _, p := range sr.Points {
			pts = append(pts, px(p.t), a.y(p.Value))
		}
		cv.line(chartPalette[i], 3, pts...)
		ends[i] = a.y(sr.Points[len(sr.Points)-1].Value)
	}
	labels := spreadLabels(ends, 28, top, bottom)
	for i, sr := range s.Series {
		c := chartPalette[i]
		lastX := px(sr.Points[len(sr.Points)-1].t)
		if two {
			cv.line(c, 1.5, lastX, ends[i], right+8, ends[i]) // the right axis's labels sit beyond
		} else {
			cv.line(c, 1.5, lastX, ends[i], right+20, ends[i], labelX, labels[i])
		}
		cv.rect(c, labelX, labels[i]-6, 12, 12)
		cv.text(labelFace, chartTitle, labelX+18, labels[i]+7, 0,
			sr.Name+"  "+s.axisOf(sr).format(sr.Points[len(sr.Points)-1].Value, 1))
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, cv.img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
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

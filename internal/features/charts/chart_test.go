package charts

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// sampleChart is two series over a month, like a stock comparison.
func sampleChart() string {
	type pt struct {
		Date  string  `json:"date"`
		Value float64 `json:"value"`
	}
	var a, b []pt
	day := time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC)
	av, bv := 180.0, 40.0
	for i := range 21 {
		d := day.AddDate(0, 0, i*4/3)
		a = append(a, pt{d.Format("2006-01-02"), av})
		b = append(b, pt{d.Format("2006-01-02"), bv})
		av *= 1 + 0.03*float64((i*7)%5-2)/2
		bv *= 1 - 0.02*float64((i*3)%4)
	}
	args, _ := json.Marshal(map[string]any{
		"title": "NVDA vs ACMR", "subtitle": "% change over the past month, 30 Sep - 28 Oct 2026",
		"series": []any{map[string]any{"name": "NVDA", "points": a}, map[string]any{"name": "ACMR", "points": b}},
	})
	return string(args)
}

func TestRenderChart(t *testing.T) {
	s, err := parseChart(sampleChart())
	if err != nil {
		t.Fatal(err)
	}
	out, err := renderChart(s, lightTheme)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 1280 || b.Dy() != 800 {
		t.Fatalf("size %v", b)
	}
	if r, g, b, _ := img.At(5, 5).RGBA(); r>>8 != 0xff || g>>8 != 0xff || b>>8 != 0xff {
		t.Fatalf("background %x %x %x", r>>8, g>>8, b>>8)
	}
	if p := os.Getenv("CHART_SAMPLE"); p != "" { // write it out to look at
		os.WriteFile(p, out, 0o644)
	}
}

func TestParseChartChangeMode(t *testing.T) {
	s, err := parseChart(`{"title":"T","series":[{"name":"A","points":[{"date":"2026-01-02","value":120},{"date":"2026-01-01","value":100}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	p := s.Series[0].Points
	if p[0].Date != "2026-01-01" || p[0].Value != 0 || p[1].Value != 20 {
		t.Fatalf("points not sorted or not rebased: %+v", p)
	}
	if got := s.summary(); got != chartNote+"Final values: A 20.0%." {
		t.Fatalf("summary %q", got)
	}
	v, _ := parseChart(`{"title":"T","axes":{"left":{"mode":"value","unit":"$"}},"series":[{"name":"A","points":[{"date":"2026-01-01","value":-1.5},{"date":"2026-01-02","value":2}]}]}`)
	if v.left.format(-1.5, 2) != "-$1.50" || v.left.format(2, 0) != "$2" {
		t.Fatalf("value format: %q %q", v.left.format(-1.5, 2), v.left.format(2, 0))
	}
}

func TestParseChartErrors(t *testing.T) {
	pts := `[{"date":"2026-01-01","value":1},{"date":"2026-01-02","value":2}]`
	for name, args := range map[string]string{
		"not json":            `{`,
		"no title":            `{"series":[{"name":"A","points":` + pts + `}]}`,
		"no series":           `{"title":"T","series":[]}`,
		"bad mode":            `{"title":"T","axes":{"left":{"mode":"log"}},"series":[{"name":"A","points":` + pts + `}]}`,
		"bad axis":            `{"title":"T","series":[{"name":"A","axis":"middle","points":` + pts + `}]}`,
		"axes key":            `{"title":"T","axes":{"middle":{}},"series":[{"name":"A","points":` + pts + `}]}`,
		"two axes, no titles": `{"title":"T","series":[{"name":"A","points":` + pts + `},{"name":"B","axis":"right","points":` + pts + `}]}`,
		"two axes, one title": `{"title":"T","axes":{"left":{"title":"L"}},"series":[{"name":"A","points":` + pts + `},{"name":"B","axis":"right","points":` + pts + `}]}`,
		"no name":             `{"title":"T","series":[{"points":` + pts + `}]}`,
		"one point":           `{"title":"T","series":[{"name":"A","points":[{"date":"2026-01-01","value":1}]}]}`,
		"bad date":            `{"title":"T","series":[{"name":"A","points":[{"date":"1 Jan","value":1},{"date":"2026-01-02","value":2}]}]}`,
		"starts at 0":         `{"title":"T","series":[{"name":"A","points":[{"date":"2026-01-01","value":0},{"date":"2026-01-02","value":2}]}]}`,
	} {
		if _, err := parseChart(args); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	seven := `{"title":"T","series":[` + strings.Repeat(`{"name":"A","points":`+pts+`},`, 6) + `{"name":"A","points":` + pts + `}]}`
	if _, err := parseChart(seven); err == nil {
		t.Error("7 series: want an error")
	}
}

func TestNiceTicks(t *testing.T) {
	ticks, dec := niceTicks(-41.2, 6.1, 5)
	if ticks[0] != -50 || ticks[len(ticks)-1] != 10 || dec != 0 {
		t.Fatalf("ticks %v decimals %d", ticks, dec)
	}
	if _, dec := niceTicks(0.11, 0.19, 5); dec != 2 {
		t.Fatalf("decimals %d, want 2", dec)
	}
}

func TestDateTicks(t *testing.T) {
	d := func(s string) time.Time { t, _ := time.Parse("2006-01-02", s); return t }
	labels := func(ts []dateTick) string {
		var out []string
		for _, tk := range ts {
			out = append(out, tk.label)
		}
		return strings.Join(out, ",")
	}
	for _, tc := range []struct{ from, to, want string }{
		{"2026-09-30", "2026-10-28", "1 Oct,15 Oct"},
		{"2026-10-01", "2026-10-04", "1 Oct,2 Oct,3 Oct,4 Oct"},
		{"2026-11-15", "2027-03-01", "Dec,Jan 2027,Feb,Mar"},
		{"2020-06-01", "2023-06-01", "2021,2022,2023"},
	} {
		if got := labels(dateTicks(d(tc.from), d(tc.to))); got != tc.want {
			t.Errorf("%s..%s: %q, want %q", tc.from, tc.to, got, tc.want)
		}
	}
	if n := len(dateTicks(d("2000-01-01"), d("2026-01-01"))); n > 8 {
		t.Errorf("%d year ticks, want at most 8", n)
	}
}

func TestSpreadLabels(t *testing.T) {
	got := spreadLabels([]float64{100, 105, 300}, 28, 0, 1000)
	if got[1]-got[0] < 28 || got[2] != 300 {
		t.Fatalf("got %v", got)
	}
	if got := spreadLabels([]float64{995, 999}, 28, 0, 1000); got[1] > 1000 || got[1]-got[0] < 28 {
		t.Fatalf("near the bottom: %v", got)
	}
}

// Each axis has its own mode: % change is computed only for its own series.
func TestChartTwoAxes(t *testing.T) {
	s, err := parseChart(`{"title":"T","axes":{"left":{"title":"Price","mode":"value","unit":"$"},"right":{"title":"Change"}},"series":[` +
		`{"name":"A","points":[{"date":"2026-01-01","value":100},{"date":"2026-01-02","value":120}]},` +
		`{"name":"B","axis":"right","points":[{"date":"2026-01-01","value":50},{"date":"2026-01-02","value":25}]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !s.right.used || s.Series[0].Points[1].Value != 120 || s.Series[1].Points[1].Value != -50 {
		t.Fatalf("per-axis modes: %+v", s.Series)
	}
	if got := s.summary(); got != chartNote+"Final values: A $120.0, B -50.0%." {
		t.Fatalf("summary %q", got)
	}
	if _, err := renderChart(s, lightTheme); err != nil {
		t.Fatal(err)
	}
	// One axis needs no title.
	if _, err := parseChart(`{"title":"T","series":[{"name":"A","points":[{"date":"2026-01-01","value":1},{"date":"2026-01-02","value":2}]}]}`); err != nil {
		t.Fatalf("single axis without a title: %v", err)
	}
	// Series only on the right are drawn on a single axis, so no titles are needed.
	r, err := parseChart(`{"title":"T","axes":{"right":{"mode":"value","unit":"$"}},"series":[{"name":"A","axis":"right","points":[{"date":"2026-01-01","value":1},{"date":"2026-01-02","value":2}]}]}`)
	if err != nil || r.right.used || r.left.Unit != "$" || r.Series[0].Axis != "left" {
		t.Fatalf("right-only chart: %+v, %v", r, err)
	}
	// A right axis with no series on it is ignored.
	o, err := parseChart(`{"title":"T","axes":{"right":{"title":"unused"}},"series":[{"name":"A","points":[{"date":"2026-01-01","value":1},{"date":"2026-01-02","value":2}]}]}`)
	if err != nil || o.right.used {
		t.Fatalf("unused right axis: %v, %v", o.right.used, err)
	}
}

// An inverted axis puts its highest value at the bottom, and zero follows.
func TestChartInvertedAxis(t *testing.T) {
	for _, invert := range []bool{false, true} {
		a := &chartAxis{Mode: "value", Invert: invert}
		a.layout([]float64{0, 10}, 100, 500)
		hiY, loY := a.y(a.hi), a.y(a.lo)
		if invert && (hiY != 500 || loY != 100) || !invert && (hiY != 100 || loY != 500) {
			t.Errorf("invert=%v: hi at %v, lo at %v", invert, hiY, loY)
		}
		if mid := a.y(5); mid != 300 {
			t.Errorf("invert=%v: middle at %v", invert, mid)
		}
	}
}

func TestRenderTwoAxisSample(t *testing.T) {
	type pt struct {
		Date  string  `json:"date"`
		Value float64 `json:"value"`
	}
	var a, b, y []pt
	for i := range 30 {
		d := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format("2006-01-02")
		a = append(a, pt{d, 180 + 10*math.Sin(float64(i)/4)})
		b = append(b, pt{d, 120 + float64(i)*0.8})
		y = append(y, pt{d, 4.2 + 0.3*math.Cos(float64(i)/5)})
	}
	args, _ := json.Marshal(map[string]any{
		"title": "Prices vs 10-year yield", "subtitle": "October 2026",
		"axes": map[string]any{"left": map[string]any{"title": "Price ($)", "mode": "value", "unit": "$"},
			"right": map[string]any{"title": "Yield (inverted)", "mode": "value", "invert": true}},
		"series": []any{map[string]any{"name": "NVDA", "points": a}, map[string]any{"name": "AMD", "points": b},
			map[string]any{"name": "10Y", "axis": "right", "points": y}},
	})
	s, err := parseChart(string(args))
	if err != nil {
		t.Fatal(err)
	}
	out, err := renderChart(s, lightTheme)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(out)); err != nil {
		t.Fatal(err)
	}
	if p := os.Getenv("CHART_SAMPLE2"); p != "" {
		os.WriteFile(p, out, 0o644)
	}
}

// areaSample is one series of n daily points from start, rising or falling.
func areaSample(n int, rising bool) string {
	type pt struct {
		Date  string  `json:"date"`
		Value float64 `json:"value"`
	}
	var pts []pt
	v := 200.0
	for i := range n {
		d := time.Date(2025, 10, 30, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i*365/n).Format("2006-01-02")
		if rising {
			v += 0.15 + 6*math.Sin(float64(i)/7) - 5*math.Sin(float64(i)/3.3)
		} else {
			v = 240 - 1.6*float64(i) + 5*math.Sin(float64(i)/1.7)
		}
		pts = append(pts, pt{d, math.Round(v*100) / 100})
	}
	args, _ := json.Marshal(map[string]any{
		"title": "NVIDIA (NVDA)", "subtitle": "Share price",
		"axes":   map[string]any{"left": map[string]any{"mode": "value"}},
		"series": []any{map[string]any{"name": "NVDA", "points": pts}},
	})
	return string(args)
}

// A single series is drawn as an area: green if it rose, red if it fell, in
// each theme's colours, with its high and low found. The dark theme draws no
// pure white; set CHART_THEMES to a directory to write its samples.
func TestAreaStyle(t *testing.T) {
	for _, tc := range []struct {
		rising bool
		n      int
		env    string
		name   string
	}{{true, 250, "CHART_UP", "up"}, {false, 22, "CHART_DOWN", "down"}} {
		s, err := parseChart(areaSample(tc.n, tc.rising))
		if err != nil {
			t.Fatal(err)
		}
		pts := s.Series[0].Points
		if up := pts[len(pts)-1].Value >= pts[0].Value; up != tc.rising {
			t.Fatalf("sample rising=%v but ends %v from %v", tc.rising, pts[len(pts)-1].Value, pts[0].Value)
		}
		hi, lo := highLow(pts)
		for _, p := range pts {
			if p.Value > pts[hi].Value || p.Value < pts[lo].Value {
				t.Fatalf("high %v / low %v missed %v", pts[hi].Value, pts[lo].Value, p.Value)
			}
		}
		for _, th := range []*theme{lightTheme, darkTheme} {
			out, err := renderChart(s, th)
			if err != nil {
				t.Fatal(err)
			}
			img := decode(t, out)
			// Somewhere in the plot is the line's colour: up when rising, down when falling.
			want := th.up
			if !tc.rising {
				want = th.down
			}
			if countColour(img, image.Rect(100, 125, 1220, 700), want) == 0 {
				t.Errorf("rising=%v dark=%v: no line in %v", tc.rising, th == darkTheme, want)
			}
			if th == lightTheme {
				if p := os.Getenv(tc.env); p != "" {
					os.WriteFile(p, out, 0o644)
				}
				continue
			}
			if n := countColour(img, img.Bounds(), white); n > 0 {
				t.Errorf("rising=%v: %d white pixels in the dark theme", tc.rising, n)
			}
			if dir := os.Getenv("CHART_THEMES"); dir != "" {
				os.WriteFile(filepath.Join(dir, "dark-line-single-"+tc.name+".png"), out, 0o644)
			}
		}
	}
}

// Argument errors say what to fix, so the model can correct its next call.
func TestChartArgErrors(t *testing.T) {
	for args, want := range map[string]string{
		`{"title":"T","series":[{"name":"a","points":[{"date":"2026-10-01","value":"12.3"}]}]}`: "series.0.points.0.value must be a number, got string",
		`{"title":"T","axes":{"left":{"invert":"true"}},"series":[]}`:                           "axes.left.invert must be true or false, got string",
		`{"title":"T","series":[`: "not valid JSON (at character 23)",
		`[1,2]`:                   "must be a JSON object, got array",
		``:                        "title is required",
		`  `:                      "title is required",
	} {
		if _, err := parseChart(args); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", args, err, want)
		}
	}
	if _, err := parsePie(`{"title":"T","slices":[{"label":"a","value":"1"}]}`); err == nil || !strings.Contains(err.Error(), "slices.0.value must be a number") {
		t.Errorf("pie: %v", err)
	}
}

// Some models send the arguments as a JSON string holding the JSON.
func TestChartArgsDoubleEncoded(t *testing.T) {
	inner := `{"title":"T","series":[{"name":"a","points":[{"date":"2026-10-01","value":1},{"date":"2026-10-02","value":2}]}]}`
	b, _ := json.Marshal(inner)
	if s, err := parseChart(string(b)); err != nil || s.Title != "T" {
		t.Fatalf("got %v", err)
	}
}

// The caption keeps to one line: the subtitle only if it fits.
func TestChartCaption(t *testing.T) {
	for _, c := range []struct{ title, sub, want string }{
		{"NVDA vs AMD", "", "NVDA vs AMD"},
		{"NVDA vs AMD", "Past month", "NVDA vs AMD — Past month"},
		{"NVDA — 1 month", "Daily close, 30 Sep - 30 Oct 2026", "NVDA — 1 month"},
	} {
		if got := chartCaption(c.title, c.sub); got != c.want {
			t.Errorf("%q, %q: got %q", c.title, c.sub, got)
		}
	}
}

// twelvePie has 12 slices, some with labels long enough to wrap, so its
// legend is as tall as a legend gets.
func twelvePie() string {
	var sl []string
	for i := range 12 {
		label := "Slice"
		if i%3 == 0 {
			label = "A slice with a label long enough to wrap onto a second line"
		}
		sl = append(sl, `{"label":"`+label+`","value":`+strconv.Itoa(13-i)+`}`)
	}
	return `{"title":"Twelve parts","subtitle":"Share of total","slices":[` + strings.Join(sl, ",") + `]}`
}

// A source is written small at the bottom left, inside a band nothing else
// reaches, for both chart kinds; without one the band is empty, and the rest
// of the chart is the same either way.
func TestChartSource(t *testing.T) {
	h := register(t)
	dir := os.Getenv("CHART_THEMES")
	band := image.Rect(0, sourceTop, width, height)
	source := `"source":"Yahoo Finance, finance.yahoo.com/quote/NVDA/history, retrieved 28 Oct 2026",`
	for _, tc := range []struct{ name, tool, args string }{
		{"line", chartTool, sampleChart()},
		{"area", chartTool, areaSample(250, true)},
		{"pie", pieTool, alphabetPie},
		{"pie-twelve", pieTool, twelvePie()},
	} {
		with := draw(t, h, tc.tool, "{"+source+tc.args[1:])
		without := decode(t, draw(t, h, tc.tool, tc.args)).(*image.RGBA)
		img := decode(t, with).(*image.RGBA)
		inBand := func(m *image.RGBA, r image.Rectangle) int {
			return r.Dx()*r.Dy() - countColour(m, r, lightTheme.bg)
		}
		if n := inBand(without, band); n != 0 {
			t.Errorf("%s: %d pixels drawn in the source's band without a source", tc.name, n)
		}
		if inBand(img, image.Rect(38, sourceTop, 300, height)) == 0 || countColour(img, band, lightTheme.muted) == 0 {
			t.Errorf("%s: no source drawn", tc.name)
		}
		if inBand(img, image.Rect(width-200, sourceTop, width, height)) != 0 {
			t.Errorf("%s: source runs to the right edge", tc.name)
		}
		if off := img.PixOffset(0, sourceTop); !bytes.Equal(img.Pix[:off], without.Pix[:off]) {
			t.Errorf("%s: the source changed the chart above it", tc.name)
		}
		if dir != "" {
			os.WriteFile(filepath.Join(dir, "source-"+tc.name+".png"), with, 0o644)
		}
	}
}

// The source is trimmed and clamped to 80 runes.
func TestChartSourceClamp(t *testing.T) {
	long := strings.Repeat("é", 100)
	c, err := parseChart(`{"source":"  ` + long + `  ",` + sampleChart()[1:])
	if err != nil {
		t.Fatal(err)
	}
	p, err := parsePie(`{"source":"  ` + long + `  ",` + alphabetPie[1:])
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Repeat("é", 80) + "…"
	if c.Source != want || p.Source != want {
		t.Fatalf("source %q, %q", c.Source, p.Source)
	}
	if c, _ := parseChart(`{"source":"  Yahoo Finance ",` + sampleChart()[1:]); c.Source != "Yahoo Finance" {
		t.Fatalf("source %q", c.Source)
	}
}

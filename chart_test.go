package main

import (
	"bytes"
	"encoding/json"
	"image/png"
	"math"
	"os"
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
	for i := 0; i < 21; i++ {
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
	out, err := renderChart(s)
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
	if got := s.summary(); got != "Chart attached to your reply. Final values: A 20.0%." {
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
	if got := s.summary(); got != "Chart attached to your reply. Final values: A $120.0, B -50.0%." {
		t.Fatalf("summary %q", got)
	}
	if _, err := renderChart(s); err != nil {
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
	for i := 0; i < 30; i++ {
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
	out, err := renderChart(s)
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
	for i := 0; i < n; i++ {
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

// A single series is drawn as an area: green if it rose, red if it fell,
// with its high and low found.
func TestAreaStyle(t *testing.T) {
	for _, tc := range []struct {
		rising bool
		n      int
		env    string
	}{{true, 250, "CHART_UP"}, {false, 22, "CHART_DOWN"}} {
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
		out, err := renderChart(s)
		if err != nil {
			t.Fatal(err)
		}
		img, _ := png.Decode(bytes.NewReader(out))
		// Somewhere in the plot is the line's colour: green when rising, red when falling.
		want := chartUp
		if !tc.rising {
			want = chartDown
		}
		found := false
		for x := 100; x < 1220 && !found; x++ {
			for y := 125; y < 700; y++ {
				if r, g, b, _ := img.At(x, y).RGBA(); uint8(r>>8) == want.R && uint8(g>>8) == want.G && uint8(b>>8) == want.B {
					found = true
					break
				}
			}
		}
		if !found {
			t.Errorf("rising=%v: no line in %v", tc.rising, want)
		}
		if p := os.Getenv(tc.env); p != "" {
			os.WriteFile(p, out, 0o644)
		}
	}
}

package main

import (
	"bytes"
	"encoding/json"
	"image/png"
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
	if r, g, b, _ := img.At(5, 5).RGBA(); r>>8 != 0x14 || g>>8 != 0x18 || b>>8 != 0x1f {
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
	v, _ := parseChart(`{"title":"T","mode":"value","unit":"$","series":[{"name":"A","points":[{"date":"2026-01-01","value":-1.5},{"date":"2026-01-02","value":2}]}]}`)
	if v.format(-1.5, 2) != "-$1.50" || v.format(2, 0) != "$2" {
		t.Fatalf("value format: %q %q", v.format(-1.5, 2), v.format(2, 0))
	}
}

func TestParseChartErrors(t *testing.T) {
	pts := `[{"date":"2026-01-01","value":1},{"date":"2026-01-02","value":2}]`
	for name, args := range map[string]string{
		"not json":    `{`,
		"no title":    `{"series":[{"name":"A","points":` + pts + `}]}`,
		"no series":   `{"title":"T","series":[]}`,
		"bad mode":    `{"title":"T","mode":"log","series":[{"name":"A","points":` + pts + `}]}`,
		"no name":     `{"title":"T","series":[{"points":` + pts + `}]}`,
		"one point":   `{"title":"T","series":[{"name":"A","points":[{"date":"2026-01-01","value":1}]}]}`,
		"bad date":    `{"title":"T","series":[{"name":"A","points":[{"date":"1 Jan","value":1},{"date":"2026-01-02","value":2}]}]}`,
		"starts at 0": `{"title":"T","series":[{"name":"A","points":[{"date":"2026-01-01","value":0},{"date":"2026-01-02","value":2}]}]}`,
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

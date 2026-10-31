package charts

import (
	"bytes"
	"encoding/json"
	"image/png"
	"math"
	"os"
	"testing"
	"time"
)

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
		out, err := renderLine(s)
		if err != nil {
			t.Fatal(err)
		}
		img, _ := png.Decode(bytes.NewReader(out))
		// Somewhere in the plot is the line's colour: green when rising, red when falling.
		want := Up
		if !tc.rising {
			want = Down
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

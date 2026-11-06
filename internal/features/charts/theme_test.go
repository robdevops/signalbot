package charts

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/feature/featuretest"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

// registerWith registers the feature on a fake host with DARKMODE set to
// mode (unset if empty), returning what it logged.
func registerWith(t *testing.T, mode string) (*featuretest.Host, string) {
	t.Helper()
	var logs bytes.Buffer
	defer logx.SetOutput(&logs, &logs, false)()
	h := featuretest.New()
	if mode != "" {
		h.Settings["DARKMODE"] = mode
	}
	if err := Register(h); err != nil {
		t.Fatal(err)
	}
	return h, logs.String()
}

// setClock makes the clock read t until the test ends.
func setClock(t *testing.T, at time.Time) {
	t.Helper()
	old := now
	now = func() time.Time { return at }
	t.Cleanup(func() { now = old })
}

// draw runs a chart tool and returns its PNG.
func draw(t *testing.T, h *featuretest.Host, name, args string) []byte {
	t.Helper()
	_, pics, err := tool(t, h, name).Run(context.Background(), args)
	if err != nil || len(pics) != 1 {
		t.Fatalf("%s: %d pictures, %v", name, len(pics), err)
	}
	return pics[0].Data
}

// white is a colour the dark theme never draws, so finding it there means
// something bypassed the theme.
var white = color.RGBA{0xff, 0xff, 0xff, 0xff}

// decode decodes a chart's PNG.
func decode(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return img
}

// countColour counts the pixels in r that are exactly c.
func countColour(img image.Image, r image.Rectangle, c color.RGBA) int {
	r = r.Intersect(img.Bounds())
	n := 0
	if m, ok := img.(*image.Paletted); ok { // what a chart decodes to; At is slow under -race
		var is [256]bool // palette indexes that are c
		for i, p := range m.Palette {
			if p := color.RGBAModel.Convert(p).(color.RGBA); p.R == c.R && p.G == c.G && p.B == c.B {
				is[i] = true
			}
		}
		for y := r.Min.Y; y < r.Max.Y; y++ {
			for _, i := range m.Pix[m.PixOffset(r.Min.X, y):m.PixOffset(r.Max.X, y)] {
				if is[i] {
					n++
				}
			}
		}
		return n
	}
	if m, ok := img.(*image.RGBA); ok {
		for y := r.Min.Y; y < r.Max.Y; y++ {
			row := m.Pix[m.PixOffset(r.Min.X, y):m.PixOffset(r.Max.X, y)]
			for i := 0; i < len(row); i += 4 {
				if row[i] == c.R && row[i+1] == c.G && row[i+2] == c.B {
					n++
				}
			}
		}
		return n
	}
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			if p := color.RGBAModel.Convert(img.At(x, y)).(color.RGBA); p.R == c.R && p.G == c.G && p.B == c.B {
				n++
			}
		}
	}
	return n
}

// isDark says whether a chart's corner is the dark background.
func isDark(t *testing.T, data []byte) bool {
	t.Helper()
	return countColour(decode(t, data), image.Rect(5, 5, 6, 6), darkTheme.bg) == 1
}

// DARKMODE is off by default; off, on, auto and a range of times, 24-hour
// or with am/pm, in any case, are accepted quietly, and anything else is
// off with one warning naming the setting and the value.
func TestDarkModeSetting(t *testing.T) {
	for v, want := range map[string]darkHours{"": never, "off": never, "on": allDay, "auto": auto, " ON ": allDay, "Auto": auto,
		"18:00-6:00": {18 * 60, 6 * 60}, "18-6": {18 * 60, 6 * 60}, "6pm-6am": {18 * 60, 6 * 60}, "6:30PM - 6:30 am": {18*60 + 30, 6*60 + 30},
		"12am-12pm": {0, 12 * 60}, "0:00-23:59": {0, 23*60 + 59}, "9-17": {9 * 60, 17 * 60}} {
		h, logs := registerWith(t, v)
		if got := darkMode(h); got != want || logs != "" {
			t.Errorf("%q: %+v, logged %q", v, got, logs)
		}
	}
	for _, v := range []string{"dark", "18-18", "24-6", "13pm-6am", "18:60-6", "6-", "0pm-6am", "6pm"} {
		h, logs := registerWith(t, v)
		if got := darkMode(h); got != never || strings.Count(logs, "\n") != 1 || !strings.Contains(logs, "DARKMODE") || !strings.Contains(logs, strconv.Quote(v)) || !strings.Contains(logs, "18:00-6:00") {
			t.Errorf("%q: %+v, logged %q", v, got, logs)
		}
	}
	h, _ := registerWith(t, "dark")
	setClock(t, time.Date(2026, 1, 1, 23, 0, 0, 0, time.Local))
	if isDark(t, draw(t, h, "make_pie_chart", alphabetPie)) {
		t.Error("a bad value draws dark")
	}
}

// A range of times is dark from its start up to its end, wrapping midnight
// when the end is earlier, decided as each chart is drawn.
func TestDarkModeRange(t *testing.T) {
	night, day := darkHours{18 * 60, 6 * 60}, darkHours{9 * 60, 17 * 60}
	for _, tc := range []struct {
		hour, min  int
		night, day bool
	}{{18, 0, true, false}, {23, 59, true, false}, {0, 0, true, false}, {5, 59, true, false}, {6, 0, false, false}, {17, 59, false, false},
		{9, 0, false, true}, {16, 59, false, true}, {17, 0, false, false}, {8, 59, false, false}, {12, 0, false, true}} {
		setClock(t, time.Date(2026, 6, 1, tc.hour, tc.min, 0, 0, time.Local))
		if got := themeFor(night) == darkTheme; got != tc.night {
			t.Errorf("%02d:%02d: 18-6 dark %v", tc.hour, tc.min, got)
		}
		if got := themeFor(day) == darkTheme; got != tc.day {
			t.Errorf("%02d:%02d: 9-17 dark %v", tc.hour, tc.min, got)
		}
	}
	h, _ := registerWith(t, "6pm-6am")
	setClock(t, time.Date(2026, 6, 1, 22, 0, 0, 0, time.Local))
	if !isDark(t, draw(t, h, "make_chart", sampleChart())) {
		t.Error("6pm-6am at 22:00 is light")
	}
	setClock(t, time.Date(2026, 6, 1, 7, 0, 0, 0, time.Local))
	if isDark(t, draw(t, h, "make_chart", sampleChart())) {
		t.Error("6pm-6am at 07:00 is dark")
	}
}

// sixSeries is a line chart with all six line colours, crossing near the
// start so neighbouring colours can be compared.
func sixSeries() string {
	type pt struct {
		Date  string  `json:"date"`
		Value float64 `json:"value"`
	}
	var series []any
	for k, name := range []string{"AAA", "BBB", "CCC", "DDD", "EEE", "FFF"} {
		var pts []pt
		for i := range 30 {
			d := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, i).Format("2006-01-02")
			pts = append(pts, pt{d, 100 + float64(i*(k-2)) + 4*math.Sin(float64(i+k)/3)})
		}
		series = append(series, map[string]any{"name": name, "points": pts})
	}
	args, _ := json.Marshal(map[string]any{"title": "Six series", "subtitle": "% change over October 2026", "axes": map[string]any{"left": map[string]any{"mode": "change"}}, "series": series})
	return string(args)
}

// With DARKMODE on, both chart types are drawn on the dark background in the
// dark palette, with no pure white, and differ from light; off draws light
// whatever the time. The single-series dark colours are checked in
// TestAreaStyle. Set CHART_THEMES to a directory to write samples to look at.
func TestDarkCharts(t *testing.T) {
	setClock(t, time.Date(2026, 1, 1, 23, 0, 0, 0, time.Local))
	light, _ := registerWith(t, "off")
	dark, _ := registerWith(t, "on")
	dir := os.Getenv("CHART_THEMES")
	for _, tc := range []struct {
		name, tool, args string
		light            bool // also draw it light, to compare
	}{
		{"line", "make_chart", sampleChart(), true},
		{"pie", "make_pie_chart", alphabetPie, true},
		{"line-six", "make_chart", sixSeries(), false},
	} {
		d := draw(t, dark, tc.tool, tc.args)
		img := decode(t, d)
		if !isDark(t, d) {
			t.Errorf("%s: dark is not on the dark background", tc.name)
		}
		if n := countColour(img, img.Bounds(), white); n > 0 {
			t.Errorf("%s: %d white pixels in the dark theme", tc.name, n)
		}
		if countColour(img, img.Bounds(), darkTheme.palette[0]) == 0 || countColour(img, img.Bounds(), lightTheme.palette[0]) > 0 {
			t.Errorf("%s: not drawn in the dark palette", tc.name)
		}
		if dir != "" {
			os.WriteFile(filepath.Join(dir, "dark-"+tc.name+".png"), d, 0o644)
		}
		if !tc.light {
			continue
		}
		l := draw(t, light, tc.tool, tc.args)
		if isDark(t, l) || bytes.Equal(l, d) {
			t.Errorf("%s: light is dark, or the same as dark", tc.name)
		}
		if dir != "" {
			os.WriteFile(filepath.Join(dir, "light-"+tc.name+".png"), l, 0o644)
		}
	}
}

// DARKMODE=auto is dark from 21:00 to 07:00 local time, decided as each
// chart is drawn.
func TestDarkModeAuto(t *testing.T) {
	for _, tc := range []struct {
		hour, min int
		dark      bool
	}{{21, 0, true}, {6, 59, true}, {0, 0, true}, {7, 0, false}, {20, 59, false}, {12, 0, false}} {
		setClock(t, time.Date(2026, 6, 1, tc.hour, tc.min, 0, 0, time.Local))
		if got := themeFor(auto) == darkTheme; got != tc.dark {
			t.Errorf("%02d:%02d: dark %v", tc.hour, tc.min, got)
		}
		if themeFor(never) != lightTheme || themeFor(allDay) != darkTheme {
			t.Errorf("%02d:%02d: off or on follows the clock", tc.hour, tc.min)
		}
	}
	// The hour is the local time zone's, not the clock's.
	old := time.Local
	time.Local = time.FixedZone("UTC+10", 10*3600)
	t.Cleanup(func() { time.Local = old })
	setClock(t, time.Date(2026, 6, 1, 11, 30, 0, 0, time.UTC)) // 21:30 local
	if themeFor(auto) != darkTheme {
		t.Error("auto ignored the local time zone")
	}
	// One registration, drawn at night and then by day.
	h, _ := registerWith(t, "auto")
	setClock(t, time.Date(2026, 6, 1, 22, 0, 0, 0, time.Local))
	if !isDark(t, draw(t, h, "make_chart", sampleChart())) {
		t.Error("auto at 22:00 is light")
	}
	setClock(t, time.Date(2026, 6, 1, 12, 0, 0, 0, time.Local))
	if isDark(t, draw(t, h, "make_chart", sampleChart())) {
		t.Error("auto at 12:00 is dark")
	}
}

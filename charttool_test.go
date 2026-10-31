package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// Argument errors say what to fix, so the model can correct its next call.
func TestChartArgErrors(t *testing.T) {
	for args, want := range map[string]string{
		`{"title":"T","series":[{"name":"a","points":[{"date":"2026-10-01","value":"12.3"}]}]}`: "series.points.value must be a number, got string",
		`{"title":"T","axes":{"left":{"invert":"true"}},"series":[]}`:                           "axes.invert must be true or false, got string",
		`{"title":"T","series":[`: "not valid JSON (at character 23)",
		`[1,2]`:                   "must be a JSON object, got array",
		``:                        "title is required",
		`  `:                      "title is required",
	} {
		if _, err := parseChart(args); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: got %v, want %q", args, err, want)
		}
	}
	if _, err := parseDonut(`{"title":"T","slices":[{"label":"a","value":"1"}]}`); err == nil || !strings.Contains(err.Error(), "slices.value must be a number") {
		t.Errorf("donut: %v", err)
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

// The model calls the donut tool; the chart comes back as an image.
func TestDonutToolInLoop(t *testing.T) {
	res, err := chartTools()[1].Run(context.Background(), alphabetDonut)
	if err != nil || len(res.Images) != 1 || res.Images[0].Caption != "GOOGL Q3 FY2026 revenue by type ($B)" || !strings.Contains(res.Text, "Total $127.6B") {
		t.Fatalf("got %q, %d images, %v", res.Text, len(res.Images), err)
	}
}

const alphabetDonut = `{"title":"GOOGL Q3 FY2026 revenue by type ($B)","subtitle":"Share of total","unit":"$","suffix":"B","slices":[
	{"label":"Google network","value":7.4},{"label":"Google search and other","value":63.4},
	{"label":"Hedging gains losses","value":0.25},{"label":"Google cloud","value":30.75},
	{"label":"Google subscriptions platforms and devices","value":14.3},{"label":"Youtube ads","value":11.2},
	{"label":"Other bets","value":0.3}]}`

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

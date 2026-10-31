package charts

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
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
	res, err := Tools()[1].Run(context.Background(), alphabetDonut)
	if err != nil || len(res.Images) != 1 || res.Images[0].Caption != "GOOGL Q3 FY2026 revenue by type ($B)" || !strings.Contains(res.Text, "Total $127.6B") {
		t.Fatalf("got %q, %d images, %v", res.Text, len(res.Images), err)
	}
}

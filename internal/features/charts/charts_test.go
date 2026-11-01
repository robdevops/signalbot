package charts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/feature/featuretest"
)

// register registers the feature on a fake host.
func register(t *testing.T) *featuretest.Host {
	t.Helper()
	h := featuretest.New()
	if err := Register(h); err != nil {
		t.Fatal(err)
	}
	return h
}

// tool is the tool named name in the feature's only group.
func tool(t *testing.T, h *featuretest.Host, name string) feature.Tool {
	t.Helper()
	for _, tl := range h.ToolGroups()[0].Tools {
		if tl.Name == name {
			return tl
		}
	}
	t.Fatalf("no tool %s", name)
	return feature.Tool{}
}

// One group, offered on its keywords, with both chart tools; it reads only
// DARKMODE.
func TestChartsToolGroup(t *testing.T) {
	h := register(t)
	g := h.ToolGroups()
	if len(g) != 1 {
		t.Fatalf("%d groups", len(g))
	}
	if strings.Join(g[0].Names, ",") != "**charts**: line, pie" || g[0].Listing != "charts (make_chart, make_pie_chart)" || g[0].System != "" || g[0].Keywords == nil {
		t.Fatalf("group %+v", g[0])
	}
	if len(g[0].Tools) != 2 {
		t.Fatalf("%d tools", len(g[0].Tools))
	}
	for _, tl := range g[0].Tools {
		if tl.Description == "" || tl.Server != "" || tl.Run == nil || !json.Valid(tl.Schema) {
			t.Errorf("tool %s: %+v", tl.Name, tl)
		}
	}
	// Renamed from make_donut_chart, it is still drawn as a donut, and says so.
	if d := tool(t, h, "make_pie_chart").Description; !strings.Contains(d, "donut") {
		t.Errorf("pie description %q", d)
	}
	if r := h.EnvRead(); len(r) != 1 || r[0] != "DARKMODE" {
		t.Errorf("read %v", r)
	}
}

func TestWantsChart(t *testing.T) {
	kw := register(t).ToolGroups()[0].Keywords
	for text, want := range map[string]bool{
		"chart NVDA vs AMD": true, "Can you graph this?": true, "plot the yields": true,
		"visualise it": true, "VISUALIZATION please": true, "charts?": true, "plotted over time": true,
		"a pie chart": true, "donut please": true, "doughnut": true, "pied": false, "spied on": false,
		"what is up": false, "a plotless film": false, "paragraph": false,
	} {
		if got := kw.MatchString(text); got != want {
			t.Errorf("%q: got %v, want %v", text, got, want)
		}
	}
}

// The model calls the line chart tool: it is told the final values, and the
// chart comes back as a PNG with its caption and history note.
func TestChartTool(t *testing.T) {
	out, pics, err := tool(t, register(t), "make_chart").Run(context.Background(), sampleChart())
	if err != nil || !strings.HasPrefix(out, chartNote+"Final values: NVDA ") || len(pics) != 1 {
		t.Fatalf("got %q, %d pictures, %v", out, len(pics), err)
	}
	// The subtitle is too long for one line, so the caption is the title.
	if p := pics[0]; p.Caption != "NVDA vs ACMR" || p.Name != "NVDA vs ACMR" || p.HistoryNote != `[sent the chart "NVDA vs ACMR"]` {
		t.Fatalf("caption %q, note %q", p.Caption, p.HistoryNote)
	}
	img, err := png.Decode(bytes.NewReader(pics[0].Data))
	if err != nil || img.Bounds().Dx() != 1280 || img.Bounds().Dy() != 800 {
		t.Fatalf("bad PNG: %v", err)
	}
}

// The model calls the pie chart tool: it is told the total and the shares.
func TestPieTool(t *testing.T) {
	out, pics, err := tool(t, register(t), "make_pie_chart").Run(context.Background(), alphabetPie)
	if err != nil || !strings.Contains(out, "Total $127.6B") || len(pics) != 1 {
		t.Fatalf("got %q, %d pictures, %v", out, len(pics), err)
	}
	if p := pics[0]; p.Caption != "GOOGL Q3 FY2026 revenue by type ($B)" || p.Name != "GOOGL Q3 FY2026 revenue by type ($B)" || p.HistoryNote != `[sent the chart "GOOGL Q3 FY2026 revenue by type ($B)"]` {
		t.Fatalf("caption %q, note %q", p.Caption, p.HistoryNote)
	}
	if _, err := png.Decode(bytes.NewReader(pics[0].Data)); err != nil {
		t.Fatalf("not a PNG: %v", err)
	}
}

// Bad arguments go back to the model with what to fix, and the log gets the
// same reason, so an operator can see why a call keeps failing.
func TestChartToolBadArguments(t *testing.T) {
	h := register(t)
	for name, args := range map[string]string{
		"make_chart":     `{"title":"Alice's savings","series":[{"name":"Alice's account","points":[{"date":"Alice's birthday","value":1},{"date":"2026-01-02","value":2}]}]}`,
		"make_pie_chart": `{"title":"Alice's budget","slices":[{"label":"Alice's rent","value":-1},{"label":"food","value":2}]}`,
	} {
		out, pics, err := tool(t, h, name).Run(context.Background(), args)
		if !strings.HasPrefix(out, "error: ") || !strings.Contains(out, "Alice's") || pics != nil {
			t.Errorf("%s: got %q, %d pictures", name, out, len(pics))
		}
		if !errors.Is(err, errArgs) || err.Error() != errArgs.Error()+": "+strings.TrimPrefix(out, "error: ") {
			t.Errorf("%s: log error %v", name, err)
		}
	}
}

// A chart's file is named after its title, or its kind and the time when the
// title has no letter or digit.
func TestChartFileName(t *testing.T) {
	defer func(f func() time.Time) { now = f }(now)
	now = func() time.Time { return time.Date(2026, 11, 1, 22, 31, 15, 0, time.Local) }
	for title, want := range map[string]string{"NVDA, 1 month": "NVDA, 1 month", "📈 — !": "line_20261101-223115"} {
		if got := fileName("line", title); got != want {
			t.Errorf("fileName(%q) = %q, want %q", title, got, want)
		}
	}
}

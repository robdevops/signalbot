package main

import (
	"bytes"
	"context"
	"image/png"
	"math"
	"os"
	"strings"
	"testing"
)

const alphabetDonut = `{"title":"GOOGL Q3 FY2026 revenue by type ($B)","subtitle":"Share of total","unit":"$","suffix":"B","slices":[
	{"label":"Google network","value":7.4},{"label":"Google search and other","value":63.4},
	{"label":"Hedging gains losses","value":0.25},{"label":"Google cloud","value":30.75},
	{"label":"Google subscriptions platforms and devices","value":14.3},{"label":"Youtube ads","value":11.2},
	{"label":"Other bets","value":0.3}]}`

func TestParseDonut(t *testing.T) {
	s, err := parseDonut(alphabetDonut)
	if err != nil {
		t.Fatal(err)
	}
	if s.Slices[0].Label != "Google search and other" || s.Slices[len(s.Slices)-1].Label != "Hedging gains losses" {
		t.Fatalf("not largest first: %+v", s.Slices)
	}
	if got := s.centerText(); got != "$127.6B" {
		t.Fatalf("centre %q", got)
	}
	sum := 0.0
	for _, sl := range s.Slices {
		sum += sl.Value / s.total * 100
	}
	if math.Abs(sum-100) > 1e-9 {
		t.Fatalf("shares add to %v", sum)
	}
	if got := s.summary(); !strings.HasPrefix(got, chartNote+"Total $127.6B; shares: Google search and other 49.7%, Google cloud 24.1%") {
		t.Fatalf("summary %q", got)
	}
	c, _ := parseDonut(`{"title":"T","center":"Q3","slices":[{"label":"a","value":1},{"label":"b","value":3}]}`)
	if c.centerText() != "Q3" {
		t.Fatalf("centre override %q", c.centerText())
	}
}

func TestParseDonutErrors(t *testing.T) {
	for name, args := range map[string]string{
		"not json":  `{`,
		"no title":  `{"slices":[{"label":"a","value":1},{"label":"b","value":2}]}`,
		"one slice": `{"title":"T","slices":[{"label":"a","value":1}]}`,
		"negative":  `{"title":"T","slices":[{"label":"a","value":-1},{"label":"b","value":2}]}`,
		"all zero":  `{"title":"T","slices":[{"label":"a","value":0},{"label":"b","value":0}]}`,
		"no label":  `{"title":"T","slices":[{"label":" ","value":1},{"label":"b","value":2}]}`,
		"13 slices": `{"title":"T","slices":[` + strings.Repeat(`{"label":"a","value":1},`, 12) + `{"label":"a","value":1}]}`,
	} {
		if _, err := parseDonut(args); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestWrapLabel(t *testing.T) {
	f := face(chartFonts.regular, 26)
	defer f.Close()
	if got := wrapLabel(f, "Google cloud  24.1%", 510); len(got) != 1 {
		t.Fatalf("short label wrapped: %q", got)
	}
	got := wrapLabel(f, "Google subscriptions platforms and devices  11.2%", 420)
	if len(got) != 2 || textWidth(f, got[0]) > 420 || textWidth(f, got[1]) > 420 || !strings.HasSuffix(got[1], "11.2%") {
		t.Fatalf("wrap %q", got)
	}
	long := wrapLabel(f, strings.Repeat("word ", 40)+"9.9%", 300)
	if len(long) != 2 || !strings.HasSuffix(long[1], "…") || textWidth(f, long[1]) > 300 {
		t.Fatalf("overlong label %q", long)
	}
}

func TestRenderDonut(t *testing.T) {
	s, _ := parseDonut(alphabetDonut)
	out, err := renderDonut(s)
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(out))
	if err != nil || img.Bounds().Dx() != 1280 || img.Bounds().Dy() != 800 {
		t.Fatalf("bad PNG: %v", err)
	}
	if r, g, b, _ := img.At(5, 5).RGBA(); r>>8 != 0xff || g>>8 != 0xff || b>>8 != 0xff {
		t.Fatal("background is not white")
	}
	if p := os.Getenv("CHART_DONUT"); p != "" {
		os.WriteFile(p, out, 0o644)
	}
}

// The model calls the donut tool; the chart comes back as an image.
func TestDonutToolInLoop(t *testing.T) {
	o := NewOpenRouter(&Config{APIKey: "k", Model: "m"}, nil)
	out, images, caption, tool, err := o.runTool(context.Background(), ToolCall{Function: struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{donutTool, alphabetDonut}})
	if err != nil || tool != donutTool || caption != "GOOGL Q3 FY2026 revenue by type ($B)" || len(images) != 1 || !strings.Contains(out, "Total $127.6B") {
		t.Fatalf("got %q, %d images, %v", out, len(images), err)
	}
}

// The total is shortened, and a scale suffix on values that were already raw
// is not applied twice.
func TestDonutTotal(t *testing.T) {
	for args, want := range map[string]string{
		`"unit":"$","suffix":"B","slices":[{"label":"a","value":54300000000},{"label":"b","value":55117000000}]`: "$109.4B",
		`"unit":"$","suffix":"B","slices":[{"label":"a","value":54.3},{"label":"b","value":30.7}]`:               "$85B",
		`"suffix":"m","slices":[{"label":"a","value":1500},{"label":"b","value":500}]`:                           "2B",
		`"slices":[{"label":"a","value":1200000},{"label":"b","value":300000}]`:                                  "1.5M",
		`"slices":[{"label":"a","value":12},{"label":"b","value":0.55}]`:                                         "12.6",
		`"suffix":"kg","slices":[{"label":"a","value":12},{"label":"b","value":0.5}]`:                            "12.5kg",
	} {
		s, err := parseDonut(`{"title":"T",` + args + `}`)
		if err != nil || s.centerText() != want {
			t.Errorf("%s: got %q, want %q (%v)", args, s.centerText(), want, err)
		}
	}
}

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"math"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/image/font"
)

// donutTool is the built-in tool the model calls to draw a donut chart of
// shares of a total, sent to the chat as an image.
const donutTool = "make_donut_chart"

var donutToolSchema = json.RawMessage(`{
  "type": "object",
  "required": ["title", "slices"],
  "properties": {
    "title": {"type": "string", "description": "Short title, e.g. \"Revenue by segment, Q3\""},
    "subtitle": {"type": "string", "description": "e.g. \"Share of total\""},
    "slices": {"type": "array", "minItems": 2, "maxItems": 12, "items": {
      "type": "object", "required": ["label", "value"],
      "properties": {"label": {"type": "string"}, "value": {"type": "number", "minimum": 0}}
    }},
    "unit": {"type": "string", "description": "Prefix for the total in the centre, e.g. \"$\""},
    "suffix": {"type": "string", "description": "Suffix for the total, e.g. \"B\""},
    "center": {"type": "string", "description": "Text for the centre instead of the total"}
  }
}`)

const donutToolDescription = "Draw a donut chart of how a total splits into parts and attach it to your reply as an image. " +
	"Use it for proportions. Pass the raw values: the percentages and total are computed for you."

type donutSlice struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
}

type donutSpec struct {
	Title    string       `json:"title"`
	Subtitle string       `json:"subtitle"`
	Slices   []donutSlice `json:"slices"`
	Unit     string       `json:"unit"`
	Suffix   string       `json:"suffix"`
	Center   string       `json:"center"`

	total float64
}

// parseDonut reads and checks the model's arguments; errors are written for
// the model. Slices come back largest first.
func parseDonut(arguments string) (*donutSpec, error) {
	var s donutSpec
	if err := decodeArgs(donutTool, arguments, &s); err != nil {
		return nil, err
	}
	s.Title = clamp(strings.TrimSpace(s.Title), 80)
	s.Subtitle = clamp(strings.TrimSpace(s.Subtitle), 120)
	s.Unit, s.Suffix, s.Center = clamp(s.Unit, 4), clamp(s.Suffix, 4), clamp(strings.TrimSpace(s.Center), 16)
	if s.Title == "" {
		return nil, errors.New("title is required")
	}
	if len(s.Slices) < 2 || len(s.Slices) > 12 {
		return nil, errors.New("give 2 to 12 slices")
	}
	for i := range s.Slices {
		sl := &s.Slices[i]
		sl.Label = clamp(strings.TrimSpace(sl.Label), 60)
		switch {
		case sl.Label == "":
			return nil, fmt.Errorf("slice %d has no label", i+1)
		case sl.Value < 0 || math.IsInf(sl.Value, 0) || math.IsNaN(sl.Value):
			return nil, fmt.Errorf("slice %s has a negative value; slices are parts of a total", sl.Label)
		}
		s.total += sl.Value
	}
	if s.total <= 0 {
		return nil, errors.New("all slices are zero, so there is nothing to show")
	}
	sort.SliceStable(s.Slices, func(a, b int) bool { return s.Slices[a].Value > s.Slices[b].Value })
	return &s, nil
}

func (s *donutSpec) share(sl donutSlice) string {
	return fmt.Sprintf("%.1f%%", sl.Value/s.total*100)
}

// centerText is the total, e.g. "$127.6B", unless the model gave its own.
func (s *donutSpec) centerText() string {
	if s.Center != "" {
		return s.Center
	}
	t := strconv.FormatFloat(math.Round(s.total*10)/10, 'f', -1, 64)
	return s.Unit + t + s.Suffix
}

func (s *donutSpec) summary() string {
	var parts []string
	for _, sl := range s.Slices {
		parts = append(parts, sl.Label+" "+s.share(sl))
	}
	return chartNote + "Total " + s.centerText() + "; shares: " + strings.Join(parts, ", ") + "."
}

// wrapLabel splits text into at most two lines no wider than width in face
// f, at spaces, cutting the second with "…" if it still doesn't fit.
func wrapLabel(f font.Face, text string, width float64) []string {
	if textWidth(f, text) <= width {
		return []string{text}
	}
	words := strings.Fields(text)
	first := ""
	i := 0
	for ; i < len(words); i++ {
		next := strings.TrimSpace(first + " " + words[i])
		if textWidth(f, next) > width && first != "" {
			break
		}
		first = next
	}
	rest := strings.Join(words[i:], " ")
	if rest == "" {
		return []string{first}
	}
	for textWidth(f, rest) > width && len([]rune(rest)) > 1 {
		r := []rune(strings.TrimSuffix(rest, "…"))
		rest = string(r[:len(r)-1]) + "…"
	}
	return []string{first, rest}
}

// renderDonut draws the chart as a 1280×800 PNG.
func renderDonut(s *donutSpec) ([]byte, error) {
	const W, H = 1280, 800
	cv := &canvas{img: image.NewRGBA(image.Rect(0, 0, W, H))}
	cv.rect(chartBG, 0, 0, W, H)

	titleFace, subFace, legendFace := face(chartFonts.bold, 34), face(chartFonts.regular, 22), face(chartFonts.regular, 26)
	defer titleFace.Close()
	defer subFace.Close()
	defer legendFace.Close()
	cv.text(titleFace, chartTitle, 38, 46, 0, s.Title)
	if s.Subtitle != "" {
		cv.text(subFace, chartMuted, 38, 80, 0, s.Subtitle)
	}

	// The donut grows from radius 215 up to 300 when the legend leaves room,
	// and the two are centred together; a legend too wide wraps instead.
	const margin, gap, swatch, minOuter, maxOuter = 40.0, 80.0, 40.0, 215.0, 300.0
	labels := make([]string, len(s.Slices))
	textW := 0.0
	for i, sl := range s.Slices {
		labels[i] = sl.Label + "  " + s.share(sl)
		textW = max(textW, textWidth(legendFace, labels[i]))
	}
	outer := min(maxOuter, max(minOuter, (W-2*margin-gap-swatch-textW)/2))
	textW = min(textW, W-2*margin-gap-swatch-2*outer)
	inner := outer * 125 / 215
	cx := (W-(2*outer+gap+swatch+textW))/2 + outer
	const cy = 445.0

	// Slices clockwise from 12 o'clock, then a white gap along each boundary.
	angle := -math.Pi / 2
	var bounds []float64
	for i, sl := range s.Slices {
		sweep := sl.Value / s.total * 2 * math.Pi
		if sweep > 0 {
			n := max(2, int(sweep/(2*math.Pi/180)))
			var poly []float64
			for k := 0; k <= n; k++ {
				a := angle + sweep*float64(k)/float64(n)
				poly = append(poly, cx+outer*math.Cos(a), cy+outer*math.Sin(a))
			}
			for k := n; k >= 0; k-- {
				a := angle + sweep*float64(k)/float64(n)
				poly = append(poly, cx+inner*math.Cos(a), cy+inner*math.Sin(a))
			}
			cv.fill(chartPalette[i], poly)
			bounds = append(bounds, angle)
		}
		angle += sweep
	}
	if len(bounds) > 1 {
		for _, a := range bounds {
			cv.line(chartBG, 4, cx+(inner-2)*math.Cos(a), cy+(inner-2)*math.Sin(a), cx+(outer+2)*math.Cos(a), cy+(outer+2)*math.Sin(a))
		}
	}

	// The total in the hole, shrunk until it fits.
	center := s.centerText()
	for size := math.Round(44 * outer / minOuter); size >= 20; size -= 4 {
		f := face(chartFonts.bold, size)
		w := textWidth(f, center)
		if w <= 2*inner-30 || size <= 20 {
			cv.text(f, chartTitle, cx, cy+size*0.35, 0.5, center)
			f.Close()
			break
		}
		f.Close()
	}

	// The legend, rows centred on the donut, long labels on two lines.
	const lineH, rowGap = 32.0, 20.0
	lx := cx + outer + gap
	type row struct {
		lines []string
		i     int
	}
	var rows []row
	height := 0.0
	for i := range s.Slices {
		lines := wrapLabel(legendFace, labels[i], textW)
		rows = append(rows, row{lines, i})
		height += float64(len(lines)) * lineH
	}
	height += rowGap * float64(len(rows)-1)
	y := cy - height/2
	for _, r := range rows {
		cv.rect(chartPalette[r.i], lx, y+8, 16, 16)
		for k, l := range r.lines {
			cv.text(legendFace, chartTitle, lx+swatch, y+24+float64(k)*lineH, 0, l)
		}
		y += float64(len(r.lines))*lineH + rowGap
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, cv.img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

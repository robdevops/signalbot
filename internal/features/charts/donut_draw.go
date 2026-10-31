package charts

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/png"
	"math"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/image/font"

	"github.com/robdevops/signalbot/internal/kit/txt"
)

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
func (s *donutSpec) validate() error {
	s.Title = txt.Clamp(strings.TrimSpace(s.Title), 80)
	s.Subtitle = txt.Clamp(strings.TrimSpace(s.Subtitle), 120)
	s.Unit, s.Suffix, s.Center = txt.Clamp(s.Unit, 4), txt.Clamp(s.Suffix, 4), txt.Clamp(strings.TrimSpace(s.Center), 16)
	if s.Title == "" {
		return errors.New("title is required")
	}
	if len(s.Slices) < 2 || len(s.Slices) > 12 {
		return errors.New("give 2 to 12 slices")
	}
	for i := range s.Slices {
		sl := &s.Slices[i]
		sl.Label = txt.Clamp(strings.TrimSpace(sl.Label), 60)
		switch {
		case sl.Label == "":
			return fmt.Errorf("slice %d has no label", i+1)
		case sl.Value < 0 || math.IsInf(sl.Value, 0) || math.IsNaN(sl.Value):
			return fmt.Errorf("slice %s has a negative value; slices are parts of a total", sl.Label)
		}
		s.total += sl.Value
	}
	if s.total <= 0 {
		return errors.New("all slices are zero, so there is nothing to show")
	}
	sort.SliceStable(s.Slices, func(a, b int) bool { return s.Slices[a].Value > s.Slices[b].Value })
	return nil
}

func (s *donutSpec) share(sl donutSlice) string {
	return fmt.Sprintf("%.1f%%", sl.Value/s.total*100)
}

// centerText is the total, e.g. "$127.6B", unless the model gave its own. A
// K, M, B or T suffix scales the values, unless that would make the total
// absurd (1e15 or more), which means the values were already raw.
func (s *donutSpec) centerText() string {
	if s.Center != "" {
		return s.Center
	}
	v, suffix := s.total, s.Suffix
	if m, ok := map[string]float64{"K": 1e3, "M": 1e6, "B": 1e9, "T": 1e12}[strings.ToUpper(strings.TrimSpace(suffix))]; ok {
		if v*m < 1e15 {
			v *= m
		}
		suffix = ""
	}
	if suffix != "" {
		return s.Unit + oneDecimal(v) + suffix
	}
	for _, d := range []struct {
		div    float64
		letter string
	}{{1e12, "T"}, {1e9, "B"}, {1e6, "M"}, {1e3, "K"}} {
		if v >= d.div {
			return s.Unit + oneDecimal(v/d.div) + d.letter
		}
	}
	return s.Unit + oneDecimal(v)
}

// oneDecimal formats v with at most one decimal, e.g. 127.6 or 85.
func oneDecimal(v float64) string {
	return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64)
}

// Summary is the chart in words: the total and each share.
func (s *donutSpec) summary() string {
	var parts []string
	for _, sl := range s.Slices {
		parts = append(parts, sl.Label+" "+s.share(sl))
	}
	return "Total " + s.centerText() + "; shares: " + strings.Join(parts, ", ") + "."
}

// wrapLabel splits text into at most two lines no wider than width in Face
// f, at spaces, cutting the second with "…" if it still doesn't fit.
func wrapLabel(f font.Face, text string, width float64) []string {
	if TextWidth(f, text) <= width {
		return []string{text}
	}
	words := strings.Fields(text)
	first := ""
	i := 0
	for ; i < len(words); i++ {
		next := strings.TrimSpace(first + " " + words[i])
		if TextWidth(f, next) > width && first != "" {
			break
		}
		first = next
	}
	rest := strings.Join(words[i:], " ")
	if rest == "" {
		return []string{first}
	}
	for TextWidth(f, rest) > width && len([]rune(rest)) > 1 {
		r := []rune(strings.TrimSuffix(rest, "…"))
		rest = string(r[:len(r)-1]) + "…"
	}
	return []string{first, rest}
}

// renderDonut draws the chart as a 1280×800 PNG.
func renderDonut(s *donutSpec) ([]byte, error) {
	const W, H = 1280, 800
	cv := &Canvas{Img: image.NewRGBA(image.Rect(0, 0, W, H))}
	cv.Rect(BG, 0, 0, W, H)

	titleFace, subFace, legendFace := Face(Fonts.Bold, 34), Face(Fonts.Regular, 22), Face(Fonts.Regular, 26)
	defer titleFace.Close()
	defer subFace.Close()
	defer legendFace.Close()
	cv.Text(titleFace, Ink, 38, 46, 0, s.Title)
	if s.Subtitle != "" {
		cv.Text(subFace, Muted, 38, 80, 0, s.Subtitle)
	}

	// The donut grows from radius 215 up to 300 when the legend leaves room,
	// and the two are centred together; a legend too wide wraps instead.
	const margin, gap, swatch, minOuter, maxOuter = 40.0, 80.0, 40.0, 215.0, 300.0
	labels := make([]string, len(s.Slices))
	textW := 0.0
	for i, sl := range s.Slices {
		labels[i] = sl.Label + "  " + s.share(sl)
		textW = max(textW, TextWidth(legendFace, labels[i]))
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
			cv.Fill(Palette[i], poly)
			bounds = append(bounds, angle)
		}
		angle += sweep
	}
	if len(bounds) > 1 {
		for _, a := range bounds {
			cv.Line(BG, 4, cx+(inner-2)*math.Cos(a), cy+(inner-2)*math.Sin(a), cx+(outer+2)*math.Cos(a), cy+(outer+2)*math.Sin(a))
		}
	}

	// The total in the hole, shrunk until it fits.
	center := s.centerText()
	for size := math.Round(44 * outer / minOuter); size >= 20; size -= 4 {
		f := Face(Fonts.Bold, size)
		w := TextWidth(f, center)
		if w <= 2*inner-30 || size <= 20 {
			cv.Text(f, Ink, cx, cy+size*0.35, 0.5, center)
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
		cv.Rect(Palette[r.i], lx, y+8, 16, 16)
		for k, l := range r.lines {
			cv.Text(legendFace, Ink, lx+swatch, y+24+float64(k)*lineH, 0, l)
		}
		y += float64(len(r.lines))*lineH + rowGap
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, cv.Img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

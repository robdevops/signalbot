package charts

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/image/font"

	"github.com/robdevops/signalbot/internal/kit/txt"
)

// pieTool is the tool the model calls to draw a pie chart of shares of a
// total, sent to the chat as an image. It is drawn as a ring (a donut) with
// the total in the middle.
const pieTool = "make_pie_chart"

var pieToolSchema = json.RawMessage(`{
  "type": "object",
  "required": ["title", "slices"],
  "properties": {
    "title": {"type": "string", "description": "Short title, e.g. \"Revenue by segment, Q3\""},
    "subtitle": {"type": "string", "description": "e.g. \"Share of total\""},
    "source": {"type": "string", "description": "` + sourceDescription + `"},
    "slices": {"type": "array", "minItems": 2, "maxItems": 12, "items": {
      "type": "object", "required": ["label", "value"],
      "properties": {"label": {"type": "string"}, "value": {"type": "number", "minimum": 0}}
    }},
    "unit": {"type": "string", "description": "Currency symbol put before the amounts, e.g. \"$\"; leave it empty for counts"},
    "suffix": {"type": "string", "description": "The values' scale, K, M, B or T (e.g. \"B\" if 54.3 means 54.3 billion), or a unit such as \"kg\". Large totals are shortened for you"},
    "center": {"type": "string", "description": "Text for the centre instead of the total"}
  }
}`)

const pieToolDescription = "Draw a donut chart of how a total splits into parts and attach it to your reply as an image. " +
	"Use it for proportions. Pass the raw values: the legend shows each amount, and the total is computed for you. " +
	"The chart is sent alone with its title as the caption, so write no other text: it would be discarded."

type pieSlice struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
}

type pieSpec struct {
	Title    string     `json:"title"`
	Subtitle string     `json:"subtitle"`
	Source   string     `json:"source"`
	Slices   []pieSlice `json:"slices"`
	Unit     string     `json:"unit"`
	Suffix   string     `json:"suffix"`
	Center   string     `json:"center"`

	total float64
}

// parsePie reads and checks the model's arguments; errors are written for
// the model. Slices come back largest first.
func parsePie(arguments string) (*pieSpec, error) {
	var s pieSpec
	if err := decodeArgs(pieTool, arguments, &s); err != nil {
		return nil, err
	}
	s.Title = txt.Clamp(strings.TrimSpace(s.Title), 80)
	s.Subtitle = txt.Clamp(strings.TrimSpace(s.Subtitle), 120)
	s.Source = txt.Clamp(strings.TrimSpace(s.Source), 80)
	s.Unit, s.Suffix, s.Center = short(s.Unit), short(s.Suffix), txt.Clamp(strings.TrimSpace(s.Center), 16)
	if s.Title == "" {
		return nil, errors.New("title is required")
	}
	if len(s.Slices) < 2 || len(s.Slices) > 12 {
		return nil, errors.New("give 2 to 12 slices")
	}
	for i := range s.Slices {
		sl := &s.Slices[i]
		sl.Label = txt.Clamp(strings.TrimSpace(sl.Label), 60)
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

func (s *pieSpec) share(sl pieSlice) string {
	return fmt.Sprintf("%.1f%%", sl.Value/s.total*100)
}

// short is a unit or suffix of up to 4 characters; a longer one, such as a
// word, is dropped rather than cut, as the number reads fine without it.
func short(u string) string {
	if u = strings.TrimSpace(u); len([]rune(u)) > 4 {
		return ""
	}
	return u
}

// centerText is the total, e.g. "$127.6B", unless the model gave its own.
func (s *pieSpec) centerText() string {
	if s.Center != "" {
		return s.Center
	}
	return s.amount(s.total)
}

// amount formats v, e.g. "$127.6B". A K, M, B or T suffix scales the values,
// unless that would make the total absurd (1e15 or more), which means the
// values were already raw.
func (s *pieSpec) amount(v float64) string {
	suffix := s.Suffix
	if m, ok := map[string]float64{"K": 1e3, "M": 1e6, "B": 1e9, "T": 1e12}[strings.ToUpper(suffix)]; ok {
		if s.total*m < 1e15 {
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

// oneDecimal formats v with at most one decimal and commas between
// thousands, e.g. 127.6 or 54,720,710.
func oneDecimal(v float64) string {
	return commas(strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64))
}

// commas puts a comma between each group of three digits before the decimal
// point of a formatted number, e.g. "-1234567.5" becomes "-1,234,567.5".
func commas(n string) string {
	end := strings.IndexByte(n, '.')
	if end < 0 {
		end = len(n)
	}
	start := len(n) - len(strings.TrimLeft(n, "-"))
	for i := end - 3; i > start; i -= 3 {
		n = n[:i] + "," + n[i:]
	}
	return n
}

func (s *pieSpec) summary() string {
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
	return []string{first, fitText(f, rest, width)}
}

// renderPie draws the chart in theme th as a PNG.
func renderPie(s *pieSpec, th *theme) ([]byte, error) {
	return drawPie(s, th).encode()
}

// drawPie draws the chart, a ring with the total in its hole, in theme th.
func drawPie(s *pieSpec, th *theme) *canvas {
	cv := newCanvas(th, s.Title, s.Subtitle, th.muted, s.Source)
	legendFace := face(chartFonts.regular, 26)
	defer legendFace.Close()

	// The donut grows from radius 215 up to 300 when the legend leaves room,
	// and the two are centred together; a legend too wide wraps instead.
	const margin, gap, swatch, minOuter, maxOuter = 40.0, 80.0, 40.0, 215.0, 300.0
	labels := make([]string, len(s.Slices))
	textW := 0.0
	for i, sl := range s.Slices {
		labels[i] = sl.Label + "  " + s.amount(sl.Value)
		textW = max(textW, textWidth(legendFace, labels[i]))
	}
	outer := min(maxOuter, max(minOuter, (width-2*margin-gap-swatch-textW)/2))
	textW = min(textW, width-2*margin-gap-swatch-2*outer)
	inner := outer * 125 / 215
	cx := (width-(2*outer+gap+swatch+textW))/2 + outer
	const cy = 445.0

	// Slices clockwise from 12 o'clock, then a gap in the background colour along each boundary.
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
			cv.fill(th.palette[i], poly)
			bounds = append(bounds, angle)
		}
		angle += sweep
	}
	if len(bounds) > 1 {
		for _, a := range bounds {
			cv.line(th.bg, 4, cx+(inner-2)*math.Cos(a), cy+(inner-2)*math.Sin(a), cx+(outer+2)*math.Cos(a), cy+(outer+2)*math.Sin(a))
		}
	}

	// The total in the hole, shrunk until it fits.
	center := s.centerText()
	for size := math.Round(44 * outer / minOuter); size >= 20; size -= 4 {
		f := face(chartFonts.bold, size)
		w := textWidth(f, center)
		if w <= 2*inner-30 || size <= 20 {
			cv.text(f, th.title, cx, cy+size*0.35, 0.5, center)
			f.Close()
			break
		}
		f.Close()
	}

	// The legend, rows centred on the donut, long labels on two lines.
	// The gaps between rows shrink to keep a tall legend above the source,
	// and if that is not enough the smallest slices' labels go back to one
	// line, cut with "…" before their amount.
	const lineH, minGap, room = 32.0, 4.0, 2 * (sourceTop - 6 - cy)
	lx := cx + outer + gap
	rows := make([][]string, len(labels))
	legendH := 0.0
	for i := range labels {
		rows[i] = wrapLabel(legendFace, labels[i], textW)
		legendH += float64(len(rows[i])) * lineH
	}
	for i := len(rows) - 1; i >= 0 && legendH+minGap*float64(len(rows)-1) > room; i-- {
		if len(rows[i]) > 1 {
			amount := "  " + s.amount(s.Slices[i].Value)
			rows[i] = []string{fitText(legendFace, s.Slices[i].Label, textW-textWidth(legendFace, amount)) + amount}
			legendH -= lineH
		}
	}
	rowGap := min(20, max(minGap, (room-legendH)/float64(len(rows)-1)))
	y := cy - (legendH+rowGap*float64(len(rows)-1))/2
	for i, lines := range rows {
		cv.rect(th.palette[i], lx, y+8, 16, 16)
		for k, l := range lines {
			cv.text(legendFace, th.title, lx+swatch, y+24+float64(k)*lineH, 0, l)
		}
		y += float64(len(lines))*lineH + rowGap
	}
	return cv
}

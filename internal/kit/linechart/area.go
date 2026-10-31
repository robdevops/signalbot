package linechart

import (
	"github.com/robdevops/signalbot/internal/kit/draw"
	"image/color"
	"time"
)

// drawArea draws a single series as an area chart: green if it rose over the
// period, red if it fell, with a translucent fill down to the bottom of the
// plot, its high and low marked and a dot on its last point. It is styling
// only; the model isn't told.
func drawArea(cv *draw.Canvas, s *Spec, px func(time.Time) float64, top, bottom, left, right float64) {
	sr, a := s.Series[0], s.left
	pts := sr.Points
	c := draw.Up
	if pts[len(pts)-1].Value < pts[0].Value {
		c = draw.Down
	}
	line := make([]float64, 0, 2*len(pts))
	for _, p := range pts {
		line = append(line, px(p.t), a.y(p.Value))
	}
	area := append(append([]float64(nil), line...), px(pts[len(pts)-1].t), bottom, px(pts[0].t), bottom)
	cv.Fill(color.NRGBA{c.R, c.G, c.B, 0x26}, area) // about 15% opacity
	cv.Line(c, 3, line...)

	hi, lo := highLow(pts)
	markFace := draw.Face(draw.Fonts.Regular, 18)
	defer markFace.Close()
	for _, m := range []struct {
		i     int
		word  string
		above bool
	}{{hi, "High", !a.Invert}, {lo, "Low", a.Invert}} {
		p := pts[m.i]
		x, y := px(p.t), a.y(p.Value)
		decimals := 2
		if a.Mode == "change" {
			decimals = 1
		}
		lines := [2]string{m.word + " " + a.format(p.Value, decimals), p.t.Format("2 Jan 2006")}
		cv.Fill(draw.Ink, draw.Circle(x, y, 6.5))
		cv.Fill(draw.BG, draw.Circle(x, y, 4.5))
		// Above the high and below the low (swapped on an inverted axis),
		// centred on the point unless that would run off the side. Where that
		// would cross the subtitle or the date labels, it goes beside the point.
		align, tx := 0.5, x
		switch {
		case x < left+110:
			align, tx = 0, x-10
		case x > right-110:
			align, tx = 1, x+10
		}
		base := y + 30 // first line's baseline, below the point
		if m.above {
			base = y - 36
		}
		if base-18 < 90 || base+22 > bottom+8 {
			base, align, tx = y-4, 1, x-14 // to the left of the point
			if x < left+260 {
				align, tx = 0, x+14 // to the right, near the left edge
			}
		}
		// A light backing keeps the label readable where it crosses the line.
		w := max(draw.TextWidth(markFace, lines[0]), draw.TextWidth(markFace, lines[1]))
		cv.Fill(color.NRGBA{0xff, 0xff, 0xff, 0xd9}, draw.RectPoly(tx-w*align-4, base-17, w+8, 45))
		cv.Text(markFace, draw.Ink, tx, base, align, lines[0])
		cv.Text(markFace, draw.Ink, tx, base+22, align, lines[1])
	}
	// The current value beside the end dot, in the line's colour.
	last := pts[len(pts)-1]
	x, y := px(last.t), a.y(last.Value)
	cv.Fill(draw.BG, draw.Circle(x, y, 7.5))
	cv.Fill(c, draw.Circle(x, y, 5.5))
	valueFace := draw.Face(draw.Fonts.Bold, 20)
	defer valueFace.Close()
	decimals := 2
	if a.Mode == "change" {
		decimals = 1
	}
	y = min(max(y, 100), bottom-14) // keep both lines inside the image
	cv.Text(valueFace, c, x+14, y-2, 0, a.format(last.Value, decimals))
	cv.Text(markFace, draw.Muted, x+14, y+20, 0, last.t.Format("2 Jan 2006"))
}

// highLow returns the indices of the first highest and first lowest points.
func highLow(pts []Point) (hi, lo int) {
	for i, p := range pts {
		if p.Value > pts[hi].Value {
			hi = i
		}
		if p.Value < pts[lo].Value {
			lo = i
		}
	}
	return hi, lo
}

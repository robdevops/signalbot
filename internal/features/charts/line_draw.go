package charts

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"time"

	"golang.org/x/image/font"
)

// renderLine draws the chart as a 1280×800 PNG.
func renderLine(s *lineSpec) ([]byte, error) {
	const W, H = 1280, 800
	const left, top, bottom = 100.0, 125.0, 700.0
	two, single := s.right.used, len(s.Series) == 1
	right, labelX := 1000.0, 1030.0 // plot edge, end labels
	switch {
	case two:
		right, labelX = 920.0, 1010.0 // room for the right axis's labels
	case single:
		right = 1140 // room for the current value beside the end dot
	}
	cv := &Canvas{Img: image.NewRGBA(image.Rect(0, 0, W, H))}
	cv.Rect(BG, 0, 0, W, H)

	titleFace, subFace := Face(Fonts.Bold, 34), Face(Fonts.Regular, 22)
	axisFace, axisTitleFace := Face(Fonts.Regular, 24), Face(Fonts.Regular, 18)
	for _, f := range []font.Face{titleFace, subFace, axisFace, axisTitleFace} {
		defer f.Close()
	}
	cv.Text(titleFace, Ink, 38, 46, 0, s.Title)
	if s.Subtitle != "" {
		cv.Text(subFace, Muted, 38, 80, 0, s.Subtitle)
	}

	from, to := s.Series[0].Points[0].t, s.Series[0].Points[0].t
	values := map[*lineAxis][]float64{}
	for _, sr := range s.Series {
		a := s.axisOf(sr)
		for _, p := range sr.Points {
			from, to = minTime(from, p.t), maxTime(to, p.t)
			values[a] = append(values[a], p.Value)
		}
	}
	s.left.layout(values[s.left], top, bottom)
	if two {
		s.right.layout(values[s.right], top, bottom)
	}
	span := to.Sub(from).Seconds()
	if span == 0 {
		span = 1
	}
	px := func(t time.Time) float64 { return left + t.Sub(from).Seconds()/span*(right-left) }

	// Gridlines and labels from the left axis; labels only from the right.
	for _, v := range s.left.ticks {
		y := s.left.y(v)
		c := Grid
		if s.left.Mode == "change" && v == 0 {
			c = Zero
		}
		cv.Line(c, 1.5, left, y, right+10, y)
		cv.Text(axisFace, Muted, left-12, y+8, 1, s.left.format(v, s.left.decimals))
	}
	if two {
		if s.right.Mode == "change" && s.left.Mode != "change" {
			y := s.right.y(0)
			cv.Line(Zero, 1.5, left, y, right+10, y)
		}
		for _, v := range s.right.ticks {
			cv.Text(axisFace, Muted, right+14, s.right.y(v)+8, 0, s.right.format(v, s.right.decimals))
		}
	}
	if s.left.Title != "" {
		cv.Text(axisTitleFace, Muted, 38, top-14, 0, s.left.Title)
	}
	if two {
		cv.Text(axisTitleFace, Muted, labelX-10, top-14, 1, s.right.Title)
	}
	for _, tk := range dateTicks(from, to) {
		x := px(tk.t)
		cv.Line(Muted, 1.5, x, bottom+14, x, bottom+24)
		cv.Text(axisFace, Muted, x, bottom+60, 0.5, tk.label)
	}

	if single {
		drawArea(cv, s, px, top, bottom, left, right)
	} else {
		drawLines(cv, s, px, top, bottom, right, labelX, two)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, cv.Img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// drawLines draws two or more series, each with an end label.
func drawLines(cv *Canvas, s *lineSpec, px func(time.Time) float64, top, bottom, right, labelX float64, two bool) {
	labelFace := Face(Fonts.Regular, 20)
	defer labelFace.Close()
	ends := make([]float64, len(s.Series))
	for i, sr := range s.Series {
		a := s.axisOf(sr)
		pts := make([]float64, 0, 2*len(sr.Points))
		for _, p := range sr.Points {
			pts = append(pts, px(p.t), a.y(p.Value))
		}
		cv.Line(Palette[i], 3, pts...)
		ends[i] = a.y(sr.Points[len(sr.Points)-1].Value)
	}
	labels := spreadLabels(ends, 28, top, bottom)
	for i, sr := range s.Series {
		c := Palette[i]
		lastX := px(sr.Points[len(sr.Points)-1].t)
		if two {
			cv.Line(c, 1.5, lastX, ends[i], right+8, ends[i]) // the right axis's labels sit beyond
		} else {
			cv.Line(c, 1.5, lastX, ends[i], right+20, ends[i], labelX, labels[i])
		}
		cv.Rect(c, labelX, labels[i]-6, 12, 12)
		cv.Text(labelFace, Ink, labelX+18, labels[i]+7, 0,
			sr.Name+"  "+s.axisOf(sr).format(sr.Points[len(sr.Points)-1].Value, 1))
	}
}

// drawArea draws a single series as an area chart: green if it rose over the
// period, red if it fell, with a translucent fill down to the bottom of the
// plot, its high and low marked and a dot on its last point. It is styling
// only; the model isn't told.
func drawArea(cv *Canvas, s *lineSpec, px func(time.Time) float64, top, bottom, left, right float64) {
	sr, a := s.Series[0], s.left
	pts := sr.Points
	c := Up
	if pts[len(pts)-1].Value < pts[0].Value {
		c = Down
	}
	line := make([]float64, 0, 2*len(pts))
	for _, p := range pts {
		line = append(line, px(p.t), a.y(p.Value))
	}
	area := append(append([]float64(nil), line...), px(pts[len(pts)-1].t), bottom, px(pts[0].t), bottom)
	cv.Fill(color.NRGBA{c.R, c.G, c.B, 0x26}, area) // about 15% opacity
	cv.Line(c, 3, line...)

	hi, lo := highLow(pts)
	markFace := Face(Fonts.Regular, 18)
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
		cv.Fill(Ink, Circle(x, y, 6.5))
		cv.Fill(BG, Circle(x, y, 4.5))
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
		w := max(TextWidth(markFace, lines[0]), TextWidth(markFace, lines[1]))
		cv.Fill(color.NRGBA{0xff, 0xff, 0xff, 0xd9}, RectPoly(tx-w*align-4, base-17, w+8, 45))
		cv.Text(markFace, Ink, tx, base, align, lines[0])
		cv.Text(markFace, Ink, tx, base+22, align, lines[1])
	}
	// The current value beside the end dot, in the line's colour.
	last := pts[len(pts)-1]
	x, y := px(last.t), a.y(last.Value)
	cv.Fill(BG, Circle(x, y, 7.5))
	cv.Fill(c, Circle(x, y, 5.5))
	valueFace := Face(Fonts.Bold, 20)
	defer valueFace.Close()
	decimals := 2
	if a.Mode == "change" {
		decimals = 1
	}
	y = min(max(y, 100), bottom-14) // keep both lines inside the image
	cv.Text(valueFace, c, x+14, y-2, 0, a.format(last.Value, decimals))
	cv.Text(markFace, Muted, x+14, y+20, 0, last.t.Format("2 Jan 2006"))
}

// highLow returns the indices of the first highest and first lowest points.
func highLow(pts []linePoint) (hi, lo int) {
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

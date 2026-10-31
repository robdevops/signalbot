package linechart

import (
	"bytes"
	"github.com/robdevops/signalbot/internal/kit/draw"
	"image"
	"image/png"
	"time"

	"golang.org/x/image/font"
)

// Render draws the chart as a 1280×800 PNG.
func Render(s *Spec) ([]byte, error) {
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
	cv := &draw.Canvas{Img: image.NewRGBA(image.Rect(0, 0, W, H))}
	cv.Rect(draw.BG, 0, 0, W, H)

	titleFace, subFace := draw.Face(draw.Fonts.Bold, 34), draw.Face(draw.Fonts.Regular, 22)
	axisFace, axisTitleFace := draw.Face(draw.Fonts.Regular, 24), draw.Face(draw.Fonts.Regular, 18)
	for _, f := range []font.Face{titleFace, subFace, axisFace, axisTitleFace} {
		defer f.Close()
	}
	cv.Text(titleFace, draw.Ink, 38, 46, 0, s.Title)
	if s.Subtitle != "" {
		cv.Text(subFace, draw.Muted, 38, 80, 0, s.Subtitle)
	}

	from, to := s.Series[0].Points[0].t, s.Series[0].Points[0].t
	values := map[*Axis][]float64{}
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
		c := draw.Grid
		if s.left.Mode == "change" && v == 0 {
			c = draw.Zero
		}
		cv.Line(c, 1.5, left, y, right+10, y)
		cv.Text(axisFace, draw.Muted, left-12, y+8, 1, s.left.format(v, s.left.decimals))
	}
	if two {
		if s.right.Mode == "change" && s.left.Mode != "change" {
			y := s.right.y(0)
			cv.Line(draw.Zero, 1.5, left, y, right+10, y)
		}
		for _, v := range s.right.ticks {
			cv.Text(axisFace, draw.Muted, right+14, s.right.y(v)+8, 0, s.right.format(v, s.right.decimals))
		}
	}
	if s.left.Title != "" {
		cv.Text(axisTitleFace, draw.Muted, 38, top-14, 0, s.left.Title)
	}
	if two {
		cv.Text(axisTitleFace, draw.Muted, labelX-10, top-14, 1, s.right.Title)
	}
	for _, tk := range dateTicks(from, to) {
		x := px(tk.t)
		cv.Line(draw.Muted, 1.5, x, bottom+14, x, bottom+24)
		cv.Text(axisFace, draw.Muted, x, bottom+60, 0.5, tk.label)
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
func drawLines(cv *draw.Canvas, s *Spec, px func(time.Time) float64, top, bottom, right, labelX float64, two bool) {
	labelFace := draw.Face(draw.Fonts.Regular, 20)
	defer labelFace.Close()
	ends := make([]float64, len(s.Series))
	for i, sr := range s.Series {
		a := s.axisOf(sr)
		pts := make([]float64, 0, 2*len(sr.Points))
		for _, p := range sr.Points {
			pts = append(pts, px(p.t), a.y(p.Value))
		}
		cv.Line(draw.Palette[i], 3, pts...)
		ends[i] = a.y(sr.Points[len(sr.Points)-1].Value)
	}
	labels := spreadLabels(ends, 28, top, bottom)
	for i, sr := range s.Series {
		c := draw.Palette[i]
		lastX := px(sr.Points[len(sr.Points)-1].t)
		if two {
			cv.Line(c, 1.5, lastX, ends[i], right+8, ends[i]) // the right axis's labels sit beyond
		} else {
			cv.Line(c, 1.5, lastX, ends[i], right+20, ends[i], labelX, labels[i])
		}
		cv.Rect(c, labelX, labels[i]-6, 12, 12)
		cv.Text(labelFace, draw.Ink, labelX+18, labels[i]+7, 0,
			sr.Name+"  "+s.axisOf(sr).format(sr.Points[len(sr.Points)-1].Value, 1))
	}
}

package charts

import (
	"image"
	"math"
	"testing"

	"golang.org/x/image/vector"
)

// fullFill is fill as it was, across the canvas's whole width, to compare
// with: only the rows the polygon spans, since those are all it can touch.
func (cv *canvas) fullFill(p []float64) *image.RGBA {
	img := image.NewRGBA(cv.img.Bounds())
	top, bottom := math.Inf(1), math.Inf(-1)
	for i := 1; i < len(p); i += 2 {
		top, bottom = min(top, p[i]), max(bottom, p[i])
	}
	rows := image.Rect(0, int(math.Floor(top)), width, int(math.Ceil(bottom))+1).Intersect(img.Bounds())
	if rows.Empty() {
		return img
	}
	r := vector.NewRasterizer(width, rows.Dy())
	oy := float32(rows.Min.Y)
	r.MoveTo(float32(p[0]), float32(p[1])-oy)
	for i := 2; i < len(p); i += 2 {
		r.LineTo(float32(p[i]), float32(p[i+1])-oy)
	}
	r.ClosePath()
	r.Draw(img, rows, image.NewUniform(lightTheme.title), image.Point{})
	return img
}

// Filling only a shape's box shades it as filling the whole canvas did, to
// within rounding, for small shapes (a pie slice, a marker, a thin line) and
// ones at or past the canvas's edges.
func TestFillMatchesTheWholeCanvas(t *testing.T) {
	t.Parallel()
	for name, p := range map[string][]float64{
		"slice":       {640, 400, 700, 300, 712.4, 310.8, 721.9, 322.3, 728.6, 335.7},
		"thin":        {100.5, 200.25, 104.5, 200.25, 104.5, 302.75, 100.5, 302.75},
		"marker":      circle(905.3, 211.7, 6),
		"left edge":   circle(2, 400, 9),
		"right edge":  circle(1278, 400, 9),
		"off canvas":  {-50, -50, -10, -50, -10, -10, -50, -10},
		"wide":        rectPoly(38, 100, 1200.5, 1.5),
		"bottom edge": {300, 795.5, 900, 795.5, 900, 805, 300, 805},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fillMatches(t, name, p)
		})
	}
}

func fillMatches(t *testing.T, name string, p []float64) {
	{
		cv := &canvas{img: image.NewRGBA(image.Rect(0, 0, width, height)), th: lightTheme}
		cv.fill(lightTheme.title, p)
		want := cv.fullFill(p)
		worst := 0
		for i := range want.Pix {
			worst = max(worst, absDiff(cv.img.Pix[i], want.Pix[i]))
		}
		if worst > 1 {
			t.Errorf("%s: a pixel is %d off", name, worst)
		}
	}
}

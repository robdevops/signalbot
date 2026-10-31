// Package draw is the Canvas, fonts and colours the charts share.
package draw

import (
	"image"
	"image/color"
	"math"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/vector"
)

// Fonts are the bundled Go fonts, parsed once.
var Fonts = func() (f struct{ Bold, Regular *opentype.Font }) {
	var err error
	if f.Bold, err = opentype.Parse(gobold.TTF); err != nil {
		panic(err)
	}
	if f.Regular, err = opentype.Parse(goregular.TTF); err != nil {
		panic(err)
	}
	return f
}()

func Face(f *opentype.Font, size float64) font.Face {
	fc, err := opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		panic(err) // only for a bad size, which these constants are not
	}
	return fc
}

// Canvas draws anti-aliased shapes and text on an image.
type Canvas struct {
	Img *image.RGBA
}

// fill paints polygons, each given as x,y pairs, in c.
func (cv *Canvas) Fill(c color.Color, polys ...[]float64) {
	b := cv.Img.Bounds()
	r := vector.NewRasterizer(b.Dx(), b.Dy())
	for _, p := range polys {
		// Every polygon is wound the same way, so overlaps add up instead of
		// cancelling out.
		area := 0.0
		for i := 0; i < len(p); i += 2 {
			j := (i + 2) % len(p)
			area += p[i]*p[j+1] - p[j]*p[i+1]
		}
		if area < 0 {
			q := make([]float64, len(p))
			for i := 0; i < len(p); i += 2 {
				q[len(p)-2-i], q[len(p)-1-i] = p[i], p[i+1]
			}
			p = q
		}
		r.MoveTo(float32(p[0]), float32(p[1]))
		for i := 2; i < len(p); i += 2 {
			r.LineTo(float32(p[i]), float32(p[i+1]))
		}
		r.ClosePath()
	}
	r.Draw(cv.Img, b, image.NewUniform(c), image.Point{})
}

func Circle(x, y, rad float64) []float64 {
	var p []float64
	for i := 0; i < 16; i++ {
		a := float64(i) * math.Pi / 8
		p = append(p, x+rad*math.Cos(a), y+rad*math.Sin(a))
	}
	return p
}

// line draws a polyline of the given width with round joins and ends.
func (cv *Canvas) Line(c color.RGBA, width float64, pts ...float64) {
	w := width / 2
	var polys [][]float64
	for i := 0; i+3 < len(pts); i += 2 {
		x1, y1, x2, y2 := pts[i], pts[i+1], pts[i+2], pts[i+3]
		dx, dy := x2-x1, y2-y1
		l := math.Hypot(dx, dy)
		if l == 0 {
			continue
		}
		nx, ny := -dy/l*w, dx/l*w
		polys = append(polys, []float64{x1 + nx, y1 + ny, x2 + nx, y2 + ny, x2 - nx, y2 - ny, x1 - nx, y1 - ny})
	}
	if width > 1.5 {
		for i := 0; i+1 < len(pts); i += 2 {
			polys = append(polys, Circle(pts[i], pts[i+1], w))
		}
	}
	if len(polys) > 0 {
		cv.Fill(c, polys...)
	}
}

func (cv *Canvas) Rect(c color.RGBA, x, y, w, h float64) {
	cv.Fill(c, []float64{x, y, x + w, y, x + w, y + h, x, y + h})
}

// text draws s with its baseline at y; align is 0 for left, 0.5 for centre
// and 1 for right of x.
func (cv *Canvas) Text(f font.Face, c color.RGBA, x, y float64, align float64, s string) {
	d := font.Drawer{Dst: cv.Img, Src: image.NewUniform(c), Face: f}
	w := float64(d.MeasureString(s)) / 64
	d.Dot = fixed.Point26_6{X: fixed.Int26_6((x - w*align) * 64), Y: fixed.Int26_6(y * 64)}
	d.DrawString(s)
}

// TextWidth is how wide s is in pixels in Face f.
func TextWidth(f font.Face, s string) float64 {
	return float64(font.MeasureString(f, s)) / 64
}

func RectPoly(x, y, w, h float64) []float64 {
	return []float64{x, y, x + w, y, x + w, y + h, x, y + h}
}

var (
	BG    = color.RGBA{0xff, 0xff, 0xff, 0xff}
	Ink   = color.RGBA{0x11, 0x18, 0x27, 0xff}
	Muted = color.RGBA{0x6b, 0x72, 0x80, 0xff}
	Grid  = color.RGBA{0xe5, 0xe7, 0xeb, 0xff}
	Zero  = color.RGBA{0x9c, 0xa3, 0xaf, 0xff}
	Up    = color.RGBA{0x16, 0xa3, 0x4a, 0xff} // a single series that rose
	Down  = color.RGBA{0xdc, 0x26, 0x26, 0xff} // a single series that fell
	// Palette serves both chart types; line charts use the first 6.
	Palette = []color.RGBA{
		{0x25, 0x63, 0xeb, 0xff}, // blue
		{0xf9, 0x73, 0x16, 0xff}, // orange
		{0x16, 0xa3, 0x4a, 0xff}, // green
		{0xdc, 0x26, 0x26, 0xff}, // red
		{0x93, 0x33, 0xea, 0xff}, // purple
		{0x08, 0x91, 0xb2, 0xff}, // teal
		{0xca, 0x8a, 0x04, 0xff}, // amber
		{0xdb, 0x27, 0x77, 0xff}, // pink
		{0x4f, 0x46, 0xe5, 0xff}, // indigo
		{0x65, 0xa3, 0x0d, 0xff}, // lime
		{0x47, 0x55, 0x69, 0xff}, // slate
		{0x92, 0x40, 0x0e, 0xff}, // brown
	}
)

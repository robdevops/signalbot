package geo

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"math/rand/v2"

	xdraw "golang.org/x/image/draw"
)

// The pixel checks' limits, on a view scaled to analysisWidth. The
// sharpness and detail limits were tuned on test images: a street scene
// passes them with room to spare, and the same scene blurred, or a bare wall,
// doesn't.
const (
	analysisWidth = 256
	minMeanLuma   = 55
	maxMeanLuma   = 205
	maxClipped    = 0.15 // of pixels at luma ≤ 5 or ≥ 250
	minLumaSD     = 28
	minSharpness  = 100  // variance of the Laplacian
	maxSky        = 0.65 // of the top two-thirds
	maxBand       = 0.20 // of the height: a larger dark band at the bottom is a dashboard
	minEdges      = 0.02 // of pixels with a Sobel magnitude over edgeLevel
	edgeLevel     = 100
)

// The output and the panorama view.
const (
	outWidth   = 1600
	outQuality = 85
	maxOut     = 1 << 20
	viewFOV    = math.Pi / 2 // a panorama's view is 90° wide
	maxYaw     = math.Pi / 3 // and turned up to 60° either way from the capture heading
	maxPixels  = 50 << 20    // the largest photo decoded
)

// Vet runs the pixel checks on a downloaded photo and returns it ready to
// post: a 90° view of a panorama, at the capture heading turned a random
// angle by r, or a flat photo as it is, with any dashboard band cropped off,
// as a JPEG about 1600 px wide and under 1 MB, which carries no metadata.
// The error is a Reject.
func Vet(data []byte, pano bool, r *rand.Rand) ([]byte, error) {
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > maxPixels {
		return nil, RejectUnreadable
	}
	m, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, RejectUnreadable
	}
	src := image.NewRGBA(image.Rect(0, 0, cfg.Width, cfg.Height))
	xdraw.Draw(src, src.Bounds(), m, m.Bounds().Min, xdraw.Src)

	var render func(w int) *image.RGBA
	width := min(outWidth, cfg.Width)
	switch {
	case pano && math.Abs(float64(cfg.Width)/float64(cfg.Height)-2) < 0.05:
		yaw := (2*r.Float64() - 1) * maxYaw
		render = func(w int) *image.RGBA { return gnomonic(src, yaw, w, w*3/4) }
		width = min(outWidth, cfg.Width/2) // a 90° view holds a quarter of the width
	case cfg.Width < minWidth:
		return nil, RejectSmall
	default:
		render = func(w int) *image.RGBA {
			if w == cfg.Width {
				return src
			}
			return scale(src, w, w*cfg.Height/cfg.Width, xdraw.CatmullRom)
		}
	}
	small := render(analysisWidth)
	keep, err := checkPixels(small)
	if err != nil {
		return nil, err
	}
	out := render(width)
	h := int(math.Round(float64(out.Bounds().Dy()) * keep))
	return encode(out.SubImage(image.Rect(0, 0, out.Bounds().Dx(), h)))
}

// scale is src scaled to w × h.
func scale(src image.Image, w, h int, s xdraw.Scaler) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, max(w, 1), max(h, 1)))
	s.Scale(dst, dst.Bounds(), src, src.Bounds(), xdraw.Src, nil)
	return dst
}

// gnomonic is a w × h rectilinear view, 90° wide and level, of the
// equirectangular panorama src, looking yaw radians right of its centre
// (the capture heading).
func gnomonic(src *image.RGBA, yaw float64, w, h int) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	sw, sh := float64(src.Bounds().Dx()), float64(src.Bounds().Dy())
	f := float64(w) / 2 / math.Tan(viewFOV/2)
	for y := range h {
		dy := (float64(y) + 0.5 - float64(h)/2) / f
		for x := range w {
			dx := (float64(x) + 0.5 - float64(w)/2) / f
			lon := yaw + math.Atan(dx) // the ray (dx, dy, 1)
			lat := -math.Atan2(dy, math.Hypot(dx, 1))
			u := (lon/(2*math.Pi)+0.5)*sw - 0.5
			v := (0.5-lat/math.Pi)*sh - 0.5
			dst.SetRGBA(x, y, bilinear(src, u, v))
		}
	}
	return dst
}

// bilinear samples src at (u, v), wrapping round horizontally.
func bilinear(src *image.RGBA, u, v float64) color.RGBA {
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	v = math.Max(0, math.Min(float64(h-1), v))
	x0, y0 := int(math.Floor(u)), int(v)
	fx, fy := u-math.Floor(u), v-float64(y0)
	y1 := min(y0+1, h-1)
	x0 = ((x0 % w) + w) % w
	x1 := (x0 + 1) % w
	var acc [4]float64
	for _, s := range [4]struct {
		x, y int
		k    float64
	}{{x0, y0, (1 - fx) * (1 - fy)}, {x1, y0, fx * (1 - fy)}, {x0, y1, (1 - fx) * fy}, {x1, y1, fx * fy}} {
		i := src.PixOffset(s.x, s.y)
		for j := range acc {
			acc[j] += s.k * float64(src.Pix[i+j])
		}
	}
	return color.RGBA{uint8(acc[0] + 0.5), uint8(acc[1] + 0.5), uint8(acc[2] + 0.5), 255}
}

// checkPixels runs the pixel checks on m, cheapest first, and returns the
// share of its height to keep: less than 1 when a dashboard band is cropped
// off the bottom.
func checkPixels(m *image.RGBA) (float64, error) {
	w, h := m.Bounds().Dx(), m.Bounds().Dy()
	if w < 3 || h < 3 {
		return 0, RejectUnreadable
	}
	luma := make([]float64, w*h)
	var sum, sq float64
	clipped := 0
	for y := range h {
		for x := range w {
			i := m.PixOffset(x, y)
			l := 0.299*float64(m.Pix[i]) + 0.587*float64(m.Pix[i+1]) + 0.114*float64(m.Pix[i+2])
			luma[y*w+x] = l
			sum += l
			sq += l * l
			if l <= 5 || l >= 250 {
				clipped++
			}
		}
	}
	n := float64(w * h)
	mean := sum / n
	if mean < minMeanLuma || mean > maxMeanLuma || float64(clipped)/n >= maxClipped {
		return 0, RejectExposure
	}
	if math.Sqrt(max(sq/n-mean*mean, 0)) <= minLumaSD {
		return 0, RejectContrast
	}
	if laplacianVariance(luma, w, h) <= minSharpness {
		return 0, RejectBlurry
	}
	edges := sobel(luma, w, h)
	if sky(m, edges, w, h) >= maxSky {
		return 0, RejectSky
	}
	band := dashboard(luma, w, h)
	if float64(band) > maxBand*float64(h) {
		return 0, RejectDashboard
	}
	keep := h - band
	strong := 0
	for y := 1; y < keep-1; y++ {
		for x := 1; x < w-1; x++ {
			if edges[y*w+x] > edgeLevel {
				strong++
			}
		}
	}
	if float64(strong)/float64((keep-2)*(w-2)) < minEdges {
		return 0, RejectBare
	}
	return float64(keep) / float64(h), nil
}

// laplacianVariance is the variance of the 4-neighbour Laplacian of luma,
// over its inner pixels: low for a blurred photo.
func laplacianVariance(luma []float64, w, h int) float64 {
	var sum, sq float64
	for y := 1; y < h-1; y++ {
		for x := 1; x < w-1; x++ {
			i := y*w + x
			l := luma[i-1] + luma[i+1] + luma[i-w] + luma[i+w] - 4*luma[i]
			sum += l
			sq += l * l
		}
	}
	n := float64((w - 2) * (h - 2))
	mean := sum / n
	return sq/n - mean*mean
}

// sobel is the Sobel gradient magnitude of luma, 0 on the border.
func sobel(luma []float64, w, h int) []float64 {
	out := make([]float64, w*h)
	for y := 1; y < h-1; y++ {
		for x := 1; x < w-1; x++ {
			i := y*w + x
			gx := luma[i-w+1] + 2*luma[i+1] + luma[i+w+1] - luma[i-w-1] - 2*luma[i-1] - luma[i+w-1]
			gy := luma[i+w-1] + 2*luma[i+w] + luma[i+w+1] - luma[i-w-1] - 2*luma[i-w] - luma[i-w+1]
			out[i] = math.Hypot(gx, gy)
		}
	}
	return out
}

// sky is the share of the top two-thirds of m that looks like sky: smooth,
// and either blue or a bright, colourless overcast.
func sky(m *image.RGBA, edges []float64, w, h int) float64 {
	top := h * 2 / 3
	count := 0
	for y := range top {
		for x := range w {
			if edges[y*w+x] > 40 {
				continue
			}
			i := m.PixOffset(x, y)
			r, g, b := int(m.Pix[i]), int(m.Pix[i+1]), int(m.Pix[i+2])
			l := (299*r + 587*g + 114*b) / 1000
			blue := b > r+10 && b >= g && l > 100
			overcast := l > 190 && max(r, g, b)-min(r, g, b) < 25
			if blue || overcast {
				count++
			}
		}
	}
	return float64(count) / float64(top*w)
}

// dashboard is the height in rows of a dark, flat band along the bottom of
// luma, such as a car's dashboard or bonnet.
func dashboard(luma []float64, w, h int) int {
	band := 0
	for y := h - 1; y >= 0; y-- {
		var sum, sq float64
		for _, l := range luma[y*w : (y+1)*w] {
			sum += l
			sq += l * l
		}
		mean := sum / float64(w)
		if mean >= 50 || math.Sqrt(max(sq/float64(w)-mean*mean, 0)) >= 12 {
			break
		}
		band++
	}
	return band
}

// encode is m as a JPEG of quality 85, lower if that is over 1 MB.
func encode(m image.Image) ([]byte, error) {
	for _, q := range []int{outQuality, 75, 65} {
		var b bytes.Buffer
		if err := jpeg.Encode(&b, m, &jpeg.Options{Quality: q}); err != nil {
			return nil, RejectUnreadable
		}
		if b.Len() <= maxOut {
			return b.Bytes(), nil
		}
	}
	return nil, RejectUnreadable
}

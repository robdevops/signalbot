package geo

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"math/rand/v2"
	"testing"

	xdraw "golang.org/x/image/draw"
)

// The test photos are drawn, not downloaded: a street scene of sky,
// buildings with windows and a textured road, and variations on it that
// each fail one check.

// scene is the street scene, w × h, drawn with r's noise.
func scene(w, h int, r *rand.Rand) *image.RGBA {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	horizon := h / 3
	for y := range h {
		for x := range w {
			var c color.RGBA
			switch {
			case y < horizon: // sky, paler towards the horizon
				k := uint8(80 * y / horizon)
				c = color.RGBA{90 + k, 140 + k, 220, 255}
			case y < h*3/4: // buildings, each its own colour, with windows
				b := x * 7 / w
				c = color.RGBA{uint8(60 + 25*b), uint8(50 + 15*b), uint8(40 + 10*b), 255}
				if (x*40/w)%3 == 1 && ((y-horizon)*20/h)%2 == 1 {
					c = color.RGBA{30, 35, 45, 255}
				}
			default: // road with lane marks
				c = color.RGBA{105, 105, 110, 255}
				if (x*10/w)%2 == 0 && y > h*7/8 && y < h*7/8+h/40 {
					c = color.RGBA{230, 230, 230, 255}
				}
			}
			n := int(r.NormFloat64() * 10)
			m.SetRGBA(x, y, color.RGBA{clamp(int(c.R) + n), clamp(int(c.G) + n), clamp(int(c.B) + n), 255})
		}
	}
	// A tall building cuts into the sky.
	for y := h / 6; y < horizon; y++ {
		for x := w / 3; x < w/2; x++ {
			m.SetRGBA(x, y, color.RGBA{150, 120, 100, 255})
		}
	}
	return m
}

func clamp(v int) uint8 { return uint8(max(0, min(255, v))) }

// mapPixels sets every pixel of m by f of its position and colour.
func mapPixels(m *image.RGBA, f func(x, y int, c color.RGBA) color.RGBA) *image.RGBA {
	b := m.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			m.SetRGBA(x, y, f(x, y, m.RGBAAt(x, y)))
		}
	}
	return m
}

// blur is m out of focus: shrunk by k and scaled back up.
func blur(m *image.RGBA, k int) *image.RGBA {
	w, h := m.Bounds().Dx(), m.Bounds().Dy()
	return scale(scale(m, w/k, h/k, xdraw.ApproxBiLinear), w, h, xdraw.BiLinear)
}

func jpegOf(t testing.TB, m image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, m, &jpeg.Options{Quality: 92}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// testPhotos are the test photos at w × h: name, image, the Reject wanted.
func testPhotos(w, h int) []struct {
	name string
	m    *image.RGBA
	want error
} {
	r := rand.New(rand.NewPCG(1, 2))
	return []struct {
		name string
		m    *image.RGBA
		want error
	}{
		{"good", scene(w, h, r), nil},
		{"night", mapPixels(scene(w, h, r), func(_, _ int, c color.RGBA) color.RGBA {
			return color.RGBA{c.R / 6, c.G / 6, c.B / 6, 255}
		}), RejectExposure},
		{"burnt out", mapPixels(scene(w, h, r), func(_, _ int, c color.RGBA) color.RGBA {
			return color.RGBA{clamp(int(c.R) * 3), clamp(int(c.G) * 3), clamp(int(c.B) * 3), 255}
		}), RejectExposure},
		{"fog", mapPixels(scene(w, h, r), func(_, _ int, c color.RGBA) color.RGBA {
			return color.RGBA{150 + c.R/10, 150 + c.G/10, 150 + c.B/10, 255}
		}), RejectContrast},
		{"blurry", blur(scene(w, h, r), max(w/64, 4)), RejectBlurry},
		{"sky", mapPixels(scene(w, h, r), func(x, y int, c color.RGBA) color.RGBA {
			if y < h*2/3 {
				return color.RGBA{170, 200, 240, 255}
			}
			return c
		}), RejectSky},
		{"dashboard", mapPixels(scene(w, h, r), func(_, y int, c color.RGBA) color.RGBA {
			if y >= h*65/100 {
				return color.RGBA{20, 20, 22, 255}
			}
			return c
		}), RejectDashboard},
		{"bare wall", bareWall(w, h, r), RejectBare},
	}
}

// bareWall is a smooth, sharp-grained plaster wall: exposed well, with
// contrast and grain, but no edges.
func bareWall(w, h int, r *rand.Rand) *image.RGBA {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			l := 60 + 140*float64(x)/float64(w)
			n := r.NormFloat64() * 6 * float64(w) / analysisWidth
			m.SetRGBA(x, y, color.RGBA{clamp(int(l + n + 20)), clamp(int(l + n)), clamp(int(l + n - 25)), 255})
		}
	}
	return m
}

func TestCheckPixels(t *testing.T) {
	for _, tc := range testPhotos(analysisWidth, analysisWidth*3/4) {
		_, err := checkPixels(tc.m)
		if err != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestVet(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(3, 4))
	for _, tc := range testPhotos(1024, 768) {
		out, err := Vet(jpegOf(t, tc.m), false, r)
		if err != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
			continue
		}
		if err != nil {
			continue
		}
		m, format, err := image.Decode(bytes.NewReader(out))
		if err != nil || format != "jpeg" {
			t.Fatalf("%s: output %s, %v", tc.name, format, err)
		}
		if m.Bounds().Dx() != 1024 || m.Bounds().Dy() != 768 || len(out) > maxOut {
			t.Errorf("%s: output %v, %d bytes", tc.name, m.Bounds(), len(out))
		}
	}
}

func TestVetScalesAndCrops(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(5, 6))
	// A wide photo with a short dashboard band: shrunk to 1600 wide, and
	// the band cut off.
	m := mapPixels(scene(2048, 1536, r), func(_, y int, c color.RGBA) color.RGBA {
		if y >= 1536*90/100 {
			return color.RGBA{15, 15, 18, 255}
		}
		return c
	})
	out, err := Vet(jpegOf(t, m), false, r)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := jpeg.DecodeConfig(bytes.NewReader(out))
	if cfg.Width != 1600 || cfg.Height > 1200*91/100 || cfg.Height < 1200*85/100 {
		t.Errorf("output %d × %d, want 1600 wide with about the bottom 10%% cut", cfg.Width, cfg.Height)
	}
}

func TestVetRejects(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 8))
	for _, tc := range []struct {
		name string
		data []byte
		want error
	}{
		{"not an image", []byte("hello"), RejectUnreadable},
		{"truncated", jpegOf(t, scene(1280, 960, r))[:500], RejectUnreadable},
		{"narrow", jpegOf(t, scene(800, 600, r)), RejectSmall},
	} {
		if _, err := Vet(tc.data, false, r); err != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
}

// equirect is a panorama that is a street scene ahead (its centre, the
// capture heading) and blank grey all round the rest.
func equirect(w, h int, r *rand.Rand) *image.RGBA {
	pano := mapPixels(image.NewRGBA(image.Rect(0, 0, w, h)), func(_, _ int, _ color.RGBA) color.RGBA {
		return color.RGBA{128, 128, 128, 255}
	})
	front := scene(w/2, h, r)
	for y := range h {
		for x := range w / 2 {
			pano.SetRGBA(w/4+x, y, front.RGBAAt(x, y))
		}
	}
	return pano
}

func TestVetPanorama(t *testing.T) {
	t.Parallel()
	r := rand.New(rand.NewPCG(9, 10))
	data := jpegOf(t, equirect(2048, 1024, r))
	out, err := Vet(data, true, r)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := jpeg.DecodeConfig(bytes.NewReader(out))
	if cfg.Width != 1024 || cfg.Height != 768 {
		t.Errorf("view %d × %d, want 1024 × 768", cfg.Width, cfg.Height)
	}
	// A photo that isn't 2:1 is checked as a flat photo.
	if _, err := Vet(jpegOf(t, scene(1024, 768, r)), true, r); err != nil {
		t.Errorf("a pano-flagged 4:3 photo: %v", err)
	}
}

func TestGnomonic(t *testing.T) {
	// A panorama whose colour is its longitude: the view's centre column
	// shows the yaw, and its edges 45° either side.
	src := image.NewRGBA(image.Rect(0, 0, 360, 180))
	mapPixels(src, func(x, _ int, _ color.RGBA) color.RGBA { return color.RGBA{uint8(x * 255 / 359), 0, 0, 255} })
	for _, yaw := range []float64{0, math.Pi / 4, -math.Pi / 3} {
		v := gnomonic(src, yaw, 101, 75)
		lonAt := func(x int) float64 { return float64(v.RGBAAt(x, 37).R)*359/255 - 180 }
		want := yaw * 180 / math.Pi
		if got := lonAt(50); math.Abs(got-want) > 2.5 {
			t.Errorf("yaw %.0f°: centre shows %.1f°", want, got)
		}
		if got := lonAt(0); math.Abs(got-(want-45)) > 3 {
			t.Errorf("yaw %.0f°: left edge shows %.1f°, want %.1f", want, got, want-45)
		}
		if got := lonAt(100); math.Abs(got-(want+45)) > 3 {
			t.Errorf("yaw %.0f°: right edge shows %.1f°, want %.1f", want, got, want+45)
		}
	}
	// Level: the horizon is the middle row, and straight up is out of view.
	src = image.NewRGBA(image.Rect(0, 0, 360, 180))
	mapPixels(src, func(_, y int, _ color.RGBA) color.RGBA { return color.RGBA{0, uint8(y * 255 / 179), 0, 255} })
	v := gnomonic(src, 0, 100, 100)
	if g := v.RGBAAt(50, 50).G; g < 120 || g > 135 {
		t.Errorf("middle row shows latitude level %d, want about 128", g)
	}
	if g := v.RGBAAt(50, 0).G; g < 50 {
		t.Errorf("top row shows latitude level %d, want under 45° up", g)
	}
}

package img

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
	"runtime"
	"slices"
	"testing"
)

func pngBytes(t *testing.T) []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestSendable(t *testing.T) {
	data := pngBytes(t)
	big := append(slices.Clone(data), make([]byte, MaxBytes)...)
	for _, c := range []struct {
		data []byte
		want bool
	}{{data, true}, {[]byte("RIFF\x00\x00\x00\x00WEBPVP8 \x00\x00\x00\x00"), true}, {[]byte("hello"), false}, {nil, false}, {big, false}} {
		if got := Sendable(c.data); got != c.want {
			t.Errorf("Sendable(%.12q) = %v", c.data, got)
		}
	}
}

// noise is a w by h image of noise, which barely compresses.
func noise(w, h int) *image.RGBA {
	m := image.NewRGBA(image.Rect(0, 0, w, h))
	x := uint32(1)
	for i := range m.Pix {
		x = x*1664525 + 1013904223
		m.Pix[i] = byte(x >> 24)
	}
	return m
}

func encode(t *testing.T, m image.Image) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, m); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// smallLimits lowers Fit's limits for the test, by about 16 times, so the
// same paths run on images a sixteenth the size.
func smallLimits(t *testing.T) {
	old := [3]any{sendLimit, decodeLimit, sideLimit}
	sendLimit, decodeLimit, sideLimit = 64<<10, 4<<20, 256
	t.Cleanup(func() { sendLimit, decodeLimit, sideLimit = old[0].(int), old[1].(int64), old[2].(int) })
}

// The limits are the constants, which callers see.
func TestLimits(t *testing.T) {
	if sendLimit != MaxBytes || MaxBytes != 5<<20 || decodeLimit != 64<<20 || sideLimit != 2048 || MaxReceived != 12<<20 {
		t.Fatalf("limits %d, %d, %d", sendLimit, decodeLimit, sideLimit)
	}
}

// A sendable image is kept as it is; a bigger one is shrunk by a whole
// factor to at most sideLimit pixels a side as a JPEG that fits, keeping its
// shape; anything else fails, as does a canvas too big to decode safely.
func TestFit(t *testing.T) {
	smallLimits(t)
	small := pngBytes(t)
	if got, err := Fit(small); err != nil || !bytes.Equal(got, small) {
		t.Fatalf("small: %v", err)
	}
	for _, c := range []struct{ w, h, wantW, wantH int }{{200, 200, 200, 200}, {512, 200, 256, 100}, {200, 512, 100, 256}} {
		big := encode(t, noise(c.w, c.h))
		if len(big) <= sendLimit {
			t.Fatalf("%dx%d is only %d bytes", c.w, c.h, len(big))
		}
		got, err := Fit(big)
		if err != nil || !Sendable(got) || http.DetectContentType(got) != "image/jpeg" {
			t.Fatalf("%dx%d: %d bytes, %v", c.w, c.h, len(got), err)
		}
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(got))
		if err != nil || cfg.Width != c.wantW || cfg.Height != c.wantH {
			t.Fatalf("%dx%d became %dx%d, %v", c.w, c.h, cfg.Width, cfg.Height, err)
		}
	}
	if _, err := Fit([]byte("hello")); err == nil {
		t.Error("text fitted")
	}
	// A big file that isn't a valid image, and a canvas too big to decode.
	if _, err := Fit(append(slices.Clone(small[:20]), make([]byte, sendLimit)...)); err == nil {
		t.Error("a broken PNG fitted")
	}
	var huge bytes.Buffer
	gif.Encode(&huge, image.NewPaletted(image.Rect(0, 0, 2500, 1750), []color.Color{color.White}), nil)
	if _, err := Fit(pad(huge.Bytes())); err == nil || err.Error() != "image too large to shrink" {
		t.Errorf("a 4.4 MP GIF: %v", err)
	}
	if _, err := Fit(pad(encode(t, image.NewRGBA64(image.Rect(0, 0, 1000, 625))))); err == nil || err.Error() != "image too large to shrink" {
		t.Errorf("a 0.6 MP 16-bit PNG: %v", err)
	}
	if _, err := Fit(pad(encode(t, image.NewGray(image.Rect(0, 0, 1, 625))))); err == nil {
		t.Error("a 1 by 5000 image shrank to nothing")
	}
}

// pad makes data too big to send as it is, so Fit decodes it; decoders
// ignore what follows an image.
func pad(data []byte) []byte { return append(slices.Clone(data), make([]byte, sendLimit)...) }

// A photo is shrunk within a small fraction of the bot's memory: here a
// sixteenth of a 12 MP one at a sixteenth of the limits, for the race
// detector's runs; TestFitMemoryAtRealSize checks the real thing.
func TestFitMemory(t *testing.T) {
	smallLimits(t)
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewYCbCr(image.Rect(0, 0, 1000, 750), image.YCbCrSubsampleRatio420), nil); err != nil {
		t.Fatal(err)
	}
	data := pad(b.Bytes())
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got, err := Fit(data)
	runtime.ReadMemStats(&after)
	if err != nil || !Sendable(got) {
		t.Fatal(err)
	}
	if n := after.TotalAlloc - before.TotalAlloc; n > 60<<20/16 {
		t.Fatalf("allocated %d KB", n>>10)
	}
}

// A 12 MP photo is shrunk, at the real limits, within a small fraction of
// the bot's memory. (Its pixels are blank, so it is quick to make.) The race
// detector makes it take seconds, so it runs without it: CI runs this
// package's tests again without -race.
func TestFitMemoryAtRealSize(t *testing.T) {
	if raceEnabled {
		t.Skip("run without -race")
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, image.NewYCbCr(image.Rect(0, 0, 4000, 3000), image.YCbCrSubsampleRatio420), nil); err != nil {
		t.Fatal(err)
	}
	data := pad(b.Bytes())
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	got, err := Fit(data)
	runtime.ReadMemStats(&after)
	if err != nil || !Sendable(got) {
		t.Fatal(err)
	}
	if n := after.TotalAlloc - before.TotalAlloc; n > 60<<20 {
		t.Fatalf("allocated %d MB", n>>20)
	}
}

// A JPEG's EXIF orientation is applied: a photo stored sideways, its red
// half on the left, comes out upright, red on top.
func TestFitOrientation(t *testing.T) {
	m := image.NewRGBA(image.Rect(0, 0, 40, 20))
	for y := range 20 {
		for x := range 40 {
			c := color.RGBA{0, 0, 255, 255}
			if x < 20 {
				c = color.RGBA{255, 0, 0, 255}
			}
			m.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	jpeg.Encode(&b, m, &jpeg.Options{Quality: 95})
	for _, bo := range []binary.ByteOrder{binary.BigEndian, binary.LittleEndian} {
		tiff := []byte("MM\x00\x2a\x00\x00\x00\x08\x00\x01\x01\x12\x00\x03\x00\x00\x00\x01\x00\x06\x00\x00\x00\x00\x00\x00")
		if bo == binary.LittleEndian {
			tiff = []byte("II\x2a\x00\x08\x00\x00\x00\x01\x00\x12\x01\x03\x00\x01\x00\x00\x00\x06\x00\x00\x00\x00\x00\x00\x00")
		}
		seg := append([]byte("Exif\x00\x00"), tiff...)
		app1 := append([]byte{0xff, 0xe1, 0, byte(len(seg) + 2)}, seg...)
		data := append(append(slices.Clone(b.Bytes()[:2]), app1...), b.Bytes()[2:]...)
		if o := orientation(data); o != 6 {
			t.Fatalf("%v: orientation %d", bo, o)
		}
		got, err := Fit(pad(data))
		if err != nil {
			t.Fatal(err)
		}
		out, err := jpeg.Decode(bytes.NewReader(got))
		if err != nil || out.Bounds().Dx() != 20 || out.Bounds().Dy() != 40 {
			t.Fatalf("%v: %v %v", bo, out.Bounds(), err)
		}
		if r, _, bl, _ := out.At(10, 5).RGBA(); r < 0xc000 || bl > 0x4000 {
			t.Errorf("%v: top is not red: %x %x", bo, r, bl)
		}
		if r, _, bl, _ := out.At(10, 35).RGBA(); r > 0x4000 || bl < 0xc000 {
			t.Errorf("%v: bottom is not blue: %x %x", bo, r, bl)
		}
	}
	if orientation(pngBytes(t)) != 0 || orientation(b.Bytes()) != 0 {
		t.Error("orientation without EXIF")
	}
}

// Every EXIF orientation maps the upright corners to the stored ones.
func TestUpright(t *testing.T) {
	// The stored pixel shown at the upright top-left, of a 4 by 2 image.
	want := map[int][2]int{1: {0, 0}, 2: {3, 0}, 3: {3, 1}, 4: {0, 1}, 5: {0, 0}, 6: {0, 1}, 7: {3, 1}, 8: {3, 0}}
	for o, w := range want {
		if x, y := upright(0, 0, 4, 2, o); x != w[0] || y != w[1] {
			t.Errorf("orientation %d: (%d, %d)", o, x, y)
		}
	}
}

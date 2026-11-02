package img

import (
	"bytes"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"net/http"
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

// A sendable image is kept as it is; a bigger one is scaled to at most 2048
// pixels a side as a JPEG that fits, keeping its shape; anything else fails.
func TestFit(t *testing.T) {
	small := pngBytes(t)
	if got, err := Fit(small); err != nil || !bytes.Equal(got, small) {
		t.Fatalf("small: %v", err)
	}
	for _, c := range []struct{ w, h, wantW, wantH int }{{1400, 1400, 1400, 1400}, {3000, 1000, 2048, 682}, {1000, 3000, 682, 2048}} {
		big := encode(t, noise(c.w, c.h))
		if len(big) <= MaxBytes {
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
	if _, err := Fit(append(slices.Clone(small[:20]), make([]byte, MaxBytes)...)); err == nil {
		t.Error("a broken PNG fitted")
	}
	var huge bytes.Buffer
	gif.Encode(&huge, image.NewPaletted(image.Rect(0, 0, 10000, 6000), []color.Color{color.White}), nil)
	padded := append(huge.Bytes(), make([]byte, MaxBytes)...)
	if _, err := Fit(padded); err == nil || err.Error() != "image too large to shrink" {
		t.Errorf("a 60 MP canvas: %v", err)
	}
}

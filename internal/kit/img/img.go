// Package img checks images, and shrinks one too big for a model.
package img

import (
	"bytes"
	"errors"
	"image"
	_ "image/gif" // decoders for Fit
	"image/jpeg"
	_ "image/png"
	"net/http"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// MaxBytes caps one image sent to a chat or a model.
const MaxBytes = 5 << 20

// MaxReceived caps one received image the bot fetches; Fit shrinks one over
// MaxBytes.
const MaxReceived = 25 << 20

// maxPixels caps an image Fit decodes, so a small file can't claim a huge
// canvas and exhaust memory.
const maxPixels = 50_000_000

// Supported reports whether mime is an image type chat apps show and models
// take.
func Supported(mime string) bool {
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return true
	}
	return false
}

// Sendable reports whether data is an image chat apps show and models
// take, small enough to send.
func Sendable(data []byte) bool {
	return len(data) <= MaxBytes && Supported(http.DetectContentType(data))
}

// Fit is data if it is Sendable, else, for a supported image too big to
// send, the image scaled down to at most 2048 pixels on its longer side
// (less, if it still doesn't fit) as a JPEG. An animation keeps only its
// first frame.
func Fit(data []byte) ([]byte, error) {
	if Sendable(data) {
		return data, nil
	}
	if !Supported(http.DetectContentType(data)) {
		return nil, errors.New("not an image")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("unreadable image")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width*cfg.Height > maxPixels {
		return nil, errors.New("image too large to shrink")
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("unreadable image")
	}
	for _, side := range []int{2048, 1536, 1024} {
		var b bytes.Buffer
		if err := jpeg.Encode(&b, scale(src, side), &jpeg.Options{Quality: 85}); err != nil {
			return nil, err
		}
		if b.Len() <= MaxBytes {
			return b.Bytes(), nil
		}
	}
	return nil, errors.New("image too large to shrink")
}

// scale is src at most side pixels on its longer side, on white, since a
// JPEG has no transparency.
func scale(src image.Image, side int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if w > side || h > side {
		if w >= h {
			w, h = side, max(1, h*side/w)
		} else {
			w, h = max(1, w*side/h), side
		}
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(dst, dst.Bounds(), image.White, image.Point{}, draw.Src)
	draw.BiLinear.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	return dst
}

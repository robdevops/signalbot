// Package img checks images, and shrinks one too big for a model.
package img

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	_ "image/gif" // decoders for Fit
	"image/jpeg"
	_ "image/png"
	"net/http"
	"sync"

	_ "golang.org/x/image/webp"
)

// MaxBytes caps one image sent to a chat or a model.
const MaxBytes = 5 << 20

// MaxReceived caps one received image the bot fetches; Fit shrinks one over
// MaxBytes.
const MaxReceived = 12 << 20

// maxDecoded caps the memory Fit lets an image decode to, so a small file
// can't claim a huge canvas: about 21 megapixels of JPEG, 16 of RGBA.
const maxDecoded = 64 << 20

// maxSide is the most pixels Fit leaves on an image's longer side.
const maxSide = 2048

// fitting lets one Fit decode at a time, so chats can't add up their peaks.
var fitting sync.Mutex

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
// send, the image as a JPEG upright by its EXIF orientation, shrunk by a
// whole factor to at most maxSide pixels a side, on white. An animation
// keeps only its first frame.
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
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height)*bytesPerPixel(cfg.ColorModel) > maxDecoded {
		return nil, errors.New("image too large to shrink")
	}
	if k := (max(cfg.Width, cfg.Height) + maxSide - 1) / maxSide; min(cfg.Width, cfg.Height) < k {
		return nil, errors.New("image too thin to shrink") // it would shrink to nothing
	}
	fitting.Lock()
	defer fitting.Unlock()
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("unreadable image")
	}
	small := shrink(src, orientation(data))
	for _, q := range []int{85, 70, 50} {
		var b bytes.Buffer
		if err := jpeg.Encode(&b, small, &jpeg.Options{Quality: q}); err != nil {
			return nil, err
		}
		if b.Len() <= MaxBytes {
			return b.Bytes(), nil
		}
	}
	return nil, errors.New("image too large to shrink")
}

// bytesPerPixel is about what a decoded image of the model takes a pixel,
// at most.
func bytesPerPixel(m color.Model) int64 {
	switch m {
	case color.GrayModel:
		return 1
	case color.Gray16Model:
		return 2
	case color.YCbCrModel:
		return 3 // 4:4:4; subsampled JPEGs take less
	case color.RGBA64Model, color.NRGBA64Model:
		return 8
	}
	if _, ok := m.(color.Palette); ok {
		return 1
	}
	return 4
}

// shrink is src turned upright by its EXIF orientation o (1 to 8; 0 is 1),
// each block of k by k pixels averaged into one, with k the least whole
// factor that brings the longer side to maxSide, and laid on white, since a
// JPEG has no transparency. It needs no buffer beyond its result.
func shrink(src image.Image, o int) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if o >= 5 {
		w, h = h, w
	}
	k := (max(w, h) + maxSide - 1) / maxSide
	dst := image.NewRGBA(image.Rect(0, 0, w/k, h/k))
	at := func(x, y int) color.RGBA64 {
		r, g, b, a := src.At(x, y).RGBA()
		return color.RGBA64{uint16(r), uint16(g), uint16(b), uint16(a)}
	}
	if px, ok := src.(image.RGBA64Image); ok { // every decoder's image: no allocation per pixel
		at = px.RGBA64At
	}
	n := uint64(k * k)
	for y := range h / k {
		for x := range w / k {
			var r, g, bl, a uint64
			for v := y * k; v < y*k+k; v++ {
				for u := x * k; u < x*k+k; u++ {
					sx, sy := upright(u, v, b.Dx(), b.Dy(), o)
					c := at(b.Min.X+sx, b.Min.Y+sy)
					r, g, bl, a = r+uint64(c.R), g+uint64(c.G), bl+uint64(c.B), a+uint64(c.A)
				}
			}
			white := 0xffff - a/n // premultiplied, so white shows through what isn't opaque
			i := dst.PixOffset(x, y)
			dst.Pix[i], dst.Pix[i+1], dst.Pix[i+2], dst.Pix[i+3] = byte((r/n+white)>>8), byte((g/n+white)>>8), byte((bl/n+white)>>8), 0xff
		}
	}
	return dst
}

// upright maps (u, v) in the image shown upright to the pixel stored in a
// w by h image with EXIF orientation o.
func upright(u, v, w, h, o int) (int, int) {
	switch o {
	case 2:
		return w - 1 - u, v
	case 3:
		return w - 1 - u, h - 1 - v
	case 4:
		return u, h - 1 - v
	case 5:
		return v, u
	case 6:
		return v, h - 1 - u
	case 7:
		return w - 1 - v, h - 1 - u
	case 8:
		return w - 1 - v, u
	}
	return u, v
}

// orientation is a JPEG's EXIF orientation, 1 to 8, or 0 if it has none.
func orientation(data []byte) int {
	if len(data) < 4 || data[0] != 0xff || data[1] != 0xd8 {
		return 0
	}
	for i := 2; i+4 <= len(data) && data[i] == 0xff; {
		marker, size := data[i+1], int(binary.BigEndian.Uint16(data[i+2:]))
		if marker == 0xda || size < 2 || i+2+size > len(data) {
			return 0 // the image data, before any EXIF
		}
		if seg := data[i+4 : i+2+size]; marker == 0xe1 && bytes.HasPrefix(seg, []byte("Exif\x00\x00")) {
			return exifOrientation(seg[6:])
		}
		i += 2 + size
	}
	return 0
}

// exifOrientation reads tag 0x0112 from a TIFF header's first IFD.
func exifOrientation(t []byte) int {
	if len(t) < 8 {
		return 0
	}
	var bo binary.ByteOrder
	switch string(t[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 0
	}
	ifd := int(bo.Uint32(t[4:]))
	if ifd < 8 || ifd+2 > len(t) {
		return 0
	}
	for i, n := ifd+2, int(bo.Uint16(t[ifd:])); n > 0 && i+12 <= len(t); i, n = i+12, n-1 {
		if bo.Uint16(t[i:]) == 0x0112 {
			if o := int(bo.Uint16(t[i+8:])); o >= 1 && o <= 8 {
				return o
			}
			return 0
		}
	}
	return 0
}

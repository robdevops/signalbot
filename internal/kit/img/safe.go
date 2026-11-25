package img

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
)

// The largest image Safe passes: phones decode what they are sent, so a
// small file claiming a huge canvas must never reach them.
const (
	safePixels = 40_000_000
	safeSide   = 8192
)

// Safe is data, an image from a source the bot doesn't trust, made safe to
// send to a chat: it fails for anything but a PNG, JPEG, GIF or WebP whose
// header gives at most safePixels pixels and safeSide a side, and a JPEG
// loses its metadata (EXIF, which can hold where a photo was taken, XMP,
// Photoshop's IPTC, comments, and anything after the image) but keeps its
// EXIF orientation. Only headers and segments are read, never pixels; data
// is not checked for size, which is Sendable's job.
func Safe(data []byte) ([]byte, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, errors.New("not an image")
	}
	switch format {
	case "png", "jpeg", "gif", "webp":
	default:
		return nil, errors.New("not an image")
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > safeSide || cfg.Height > safeSide || int64(cfg.Width)*int64(cfg.Height) > safePixels {
		return nil, errors.New("image too large")
	}
	if format != "jpeg" {
		return data, nil
	}
	return stripJPEG(data)
}

// stripJPEG is a JPEG without its metadata segments, walking its markers
// and copying its scans as they are. An EXIF orientation other than upright
// is kept, in a new EXIF segment that holds nothing else.
func stripJPEG(data []byte) ([]byte, error) {
	bad := errors.New("malformed JPEG")
	if len(data) < 2 || data[0] != 0xff || data[1] != 0xd8 {
		return nil, bad
	}
	out := append(make([]byte, 0, len(data)), 0xff, 0xd8)
	oriented := false
	for i := 2; ; {
		if i >= len(data) || data[i] != 0xff {
			return nil, bad
		}
		for i < len(data) && data[i] == 0xff { // fill bytes may come before a marker
			i++
		}
		if i >= len(data) {
			return nil, bad
		}
		marker := data[i]
		i++
		switch {
		case marker == 0xd9: // EOI: what follows, such as another image, is dropped
			return append(out, 0xff, 0xd9), nil
		case marker == 0x01 || marker >= 0xd0 && marker <= 0xd7: // no length
			out = append(out, 0xff, marker)
			continue
		}
		if i+2 > len(data) {
			return nil, bad
		}
		size := int(binary.BigEndian.Uint16(data[i:]))
		if size < 2 || i+size > len(data) {
			return nil, bad
		}
		payload := data[i+2 : i+size]
		i += size
		switch {
		case marker == 0xe1: // EXIF or XMP
			if !oriented && bytes.HasPrefix(payload, []byte("Exif\x00\x00")) {
				if o := exifOrientation(payload[6:]); o > 1 {
					out = append(out, orientationSegment(o)...)
					oriented = true
				}
			}
			continue
		case marker == 0xed, marker == 0xfe: // Photoshop's IPTC, a comment
			continue
		case marker == 0xe2 && bytes.HasPrefix(payload, []byte("MPF\x00")): // points at images after EOI
			continue
		}
		out = append(out, 0xff, marker)
		out = append(out, data[i-size:i]...)
		if marker == 0xda { // a scan: its data runs to the next marker but a restart
			j := i
			for ; j+1 < len(data); j++ {
				if next := data[j+1]; data[j] == 0xff && next != 0 && (next < 0xd0 || next > 0xd7) {
					break
				}
			}
			if j+1 >= len(data) {
				return nil, bad
			}
			out = append(out, data[i:j]...)
			i = j
		}
	}
}

// orientationSegment is an APP1 segment whose EXIF holds only orientation o.
func orientationSegment(o int) []byte {
	seg := []byte("\xff\xe1\x00\x00Exif\x00\x00MM\x00\x2a\x00\x00\x00\x08\x00\x01\x01\x12\x00\x03\x00\x00\x00\x01\x00\x00\x00\x00\x00\x00\x00\x00")
	binary.BigEndian.PutUint16(seg[2:], uint16(len(seg)-2))
	binary.BigEndian.PutUint16(seg[4+6+8+2+8:], uint16(o))
	return seg
}

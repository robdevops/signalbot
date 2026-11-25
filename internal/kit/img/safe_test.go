package img

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"slices"
	"testing"
)

// segment is a JPEG marker segment holding payload.
func segment(marker byte, payload []byte) []byte {
	return append([]byte{0xff, marker, byte((len(payload) + 2) >> 8), byte(len(payload) + 2)}, payload...)
}

// withSegments is a JPEG with segs put just after its start.
func withSegments(jpg []byte, segs ...[]byte) []byte {
	out := slices.Clone(jpg[:2])
	for _, s := range segs {
		out = append(out, s...)
	}
	return append(out, jpg[2:]...)
}

func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := jpeg.Encode(&b, noise(w, h), &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// pngClaiming is a 2 by 2 PNG whose header claims w by h, with the header's
// checksum right, so only the pixels give it away.
func pngClaiming(t *testing.T, w, h int) []byte {
	data := pngBytes(t)
	binary.BigEndian.PutUint32(data[16:], uint32(w))
	binary.BigEndian.PutUint32(data[20:], uint32(h))
	binary.BigEndian.PutUint32(data[29:], crc32.ChecksumIEEE(data[12:29]))
	return data
}

// jpegClaiming is a small JPEG whose frame header claims w by h.
func jpegClaiming(t *testing.T, w, h int) []byte {
	data := jpegBytes(t, 8, 8)
	i := bytes.Index(data, []byte{0xff, 0xc0})
	if i < 0 {
		t.Fatal("no frame header")
	}
	binary.BigEndian.PutUint16(data[i+5:], uint16(h))
	binary.BigEndian.PutUint16(data[i+7:], uint16(w))
	return data
}

// gpsExif is an EXIF segment's payload with orientation 6 and a GPS
// latitude of 37° 48′ S.
func gpsExif() []byte {
	var t bytes.Buffer
	be := binary.BigEndian
	t.WriteString("MM\x00\x2a\x00\x00\x00\x08")
	binary.Write(&t, be, uint16(2)) // IFD0 at 8
	binary.Write(&t, be, [6]uint16{0x0112, 3, 0, 1, 6, 0})
	binary.Write(&t, be, []uint16{0x8825, 4, 0, 1})
	binary.Write(&t, be, uint32(38)) // the GPS IFD
	binary.Write(&t, be, uint32(0))
	binary.Write(&t, be, uint16(2)) // the GPS IFD at 38
	binary.Write(&t, be, []uint16{0x0001, 2, 0, 2})
	t.WriteString("S\x00\x00\x00")
	binary.Write(&t, be, []uint16{0x0002, 5, 0, 3})
	binary.Write(&t, be, uint32(68)) // the rationals
	binary.Write(&t, be, uint32(0))
	binary.Write(&t, be, []uint32{37, 1, 48, 1, 0, 1})
	return append([]byte("Exif\x00\x00"), t.Bytes()...)
}

// markers lists a JPEG's markers up to its first scan.
func markers(data []byte) []byte {
	var ms []byte
	for i := 2; i+4 <= len(data) && data[i] == 0xff && data[i+1] != 0xda; i += 2 + int(binary.BigEndian.Uint16(data[i+2:])) {
		ms = append(ms, data[i+1])
	}
	return ms
}

func TestSafe(t *testing.T) {
	photo := jpegBytes(t, 64, 48)
	gps := gpsExif()
	xmp := append([]byte("http://ns.adobe.com/xap/1.0/\x00"), "<x:xmpmeta>Alice</x:xmpmeta>"...)
	iptc := []byte("Photoshop 3.0\x008BIM\x04\x04\x00\x00\x00\x00\x00\x08\x1c\x02\x5a\x00\x03Bob")
	// A comment between the scan and the end, and a second image after it.
	late := slices.Concat(photo[:len(photo)-2], segment(0xfe, []byte("Bob was here")), []byte{0xff, 0xd9}, photo)
	var g bytes.Buffer
	gif.Encode(&g, image.NewPaletted(image.Rect(0, 0, 3, 3), color.Palette{color.White, color.Black}), nil)

	for _, c := range []struct {
		name    string
		data    []byte
		wantErr string
		same    bool // comes out as it went in
		strip   bool // a JPEG that comes out without its metadata
		orient  int  // the orientation it comes out with
		gone    [][]byte
	}{
		{name: "png", data: pngBytes(t), same: true},
		{name: "gif", data: g.Bytes(), same: true},
		{name: "png at the limits", data: pngClaiming(t, safeSide, safePixels/safeSide), same: true},
		{name: "png bomb", data: pngClaiming(t, 100_000, 100_000), wantErr: "image too large"},
		{name: "png too many pixels", data: pngClaiming(t, safeSide, safePixels/safeSide+1), wantErr: "image too large"},
		{name: "png too wide", data: pngClaiming(t, safeSide+1, 10), wantErr: "image too large"},
		{name: "png too tall", data: pngClaiming(t, 10, safeSide+1), wantErr: "image too large"},
		{name: "jpeg bomb", data: jpegClaiming(t, 8000, 8000), wantErr: "image too large"},
		{name: "text", data: []byte("hello"), wantErr: "not an image"},
		{name: "empty", data: nil, wantErr: "not an image"},
		{name: "bmp", data: []byte("BM\x3a\x00\x00\x00\x00\x00\x00\x00\x36\x00\x00\x00\x28\x00\x00\x00\x01\x00\x00\x00\x01\x00\x00\x00\x01\x00\x18\x00"), wantErr: "not an image"},
		{name: "truncated jpeg", data: photo[:len(photo)/2], wantErr: "malformed JPEG"},
		{name: "plain jpeg", data: photo, strip: true},
		{name: "jpeg with metadata", data: withSegments(photo, segment(0xe1, gps), segment(0xe1, xmp), segment(0xed, iptc), segment(0xfe, []byte("Alice's"))), strip: true, orient: 6,
			gone: [][]byte{gps[len(gps)-24:], []byte("S\x00\x00\x00"), []byte("xmpmeta"), []byte("Alice"), []byte("Bob"), []byte("Photoshop")}},
		{name: "jpeg with late comment and trailer", data: late, strip: true, gone: [][]byte{[]byte("Bob")}},
		{name: "jpeg with fill bytes", data: withSegments(photo, []byte{0xff, 0xff}, segment(0xfe, []byte("Alice"))), strip: true, gone: [][]byte{[]byte("Alice")}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := Safe(c.data)
			if c.wantErr != "" {
				if err == nil || err.Error() != c.wantErr {
					t.Fatalf("%d bytes, %v", len(got), err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.same && !bytes.Equal(got, c.data) {
				t.Fatal("changed")
			}
			for _, s := range c.gone {
				if bytes.Contains(got, s) {
					t.Errorf("still has %q", s)
				}
			}
			if !c.strip {
				return
			}
			if o := orientation(got); o != c.orient {
				t.Errorf("orientation %d", o)
			}
			for _, m := range markers(got) {
				if m == 0xed || m == 0xfe || m == 0xe1 && c.orient == 0 {
					t.Errorf("still has marker %x", m)
				}
			}
			if n := bytes.Count(got, []byte{0xff, 0xd8}); n != 1 {
				t.Errorf("%d images", n)
			}
			want, err := jpeg.Decode(bytes.NewReader(photo))
			if err != nil {
				t.Fatal(err)
			}
			out, err := jpeg.Decode(bytes.NewReader(got))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(out.(*image.YCbCr).Y, want.(*image.YCbCr).Y) || !bytes.Equal(out.(*image.YCbCr).Cb, want.(*image.YCbCr).Cb) {
				t.Error("pixels changed")
			}
		})
	}
}

// The orientation segment Safe writes reads back as the orientation it keeps.
func TestOrientationSegment(t *testing.T) {
	photo := jpegBytes(t, 8, 8)
	for o := 1; o <= 8; o++ {
		if got := orientation(withSegments(photo, orientationSegment(o))); got != o {
			t.Errorf("wrote %d, read %d", o, got)
		}
	}
}

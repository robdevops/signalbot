package img

import (
	"bytes"
	"image"
	"image/png"
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

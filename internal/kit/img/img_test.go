package img

import (
	"bytes"
	"image"
	"image/png"
	"strings"
	"testing"
)

func pngBytes(t *testing.T) []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestAttachment(t *testing.T) {
	data := pngBytes(t)
	if got := Attachment(data, "image1"); !strings.HasPrefix(got, "data:image/png;filename=image1.png;base64,") {
		t.Errorf("Attachment %q", got[:50])
	}
}

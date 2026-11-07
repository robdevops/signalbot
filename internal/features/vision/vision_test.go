package vision

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/feature/featuretest"
	"github.com/robdevops/signalbot/internal/kit/img"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

var (
	photo   = feature.Media{ID: "photo1.png"}
	quoted  = feature.Media{ID: "thumb1.jpg", Quoted: true}
	sticker = feature.Media{ID: "00aabbccddeeff00:2", Sticker: true, Label: `: 🕵️ from "My Daily Life"`}
	webp    = []byte("RIFF\x00\x00\x00\x00WEBPVP8 \x00\x00\x00\x00")
)

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// host is a fake host whose model sees images or not, serving a PNG for an
// image and a WebP for a sticker, and recording what was fetched.
func host(t *testing.T, see bool, fetched *[]int) *featuretest.Host {
	t.Helper()
	h := featuretest.New()
	h.Bot = feature.Info{Name: "Botty", Model: "x/test-model", Images: see}
	data := pngBytes(t)
	h.Fetch = func(m feature.Message, i int) ([]byte, error) {
		*fetched = append(*fetched, i)
		if m.Media[i].Sticker {
			return webp, nil
		}
		return data, nil
	}
	if err := Register(h); err != nil {
		t.Fatal(err)
	}
	return h
}

func turn(text string, media ...feature.Media) feature.Message {
	return feature.Message{Chat: feature.Chat{User: "u-alice"}, Sender: "u-alice", Name: "Alice", Text: text, Media: media, Addressed: true}
}

func additionsOf(t *testing.T, h *featuretest.Host, tn feature.Message) feature.Additions {
	t.Helper()
	as := h.Turn(context.Background(), tn)
	if len(as) != 1 {
		t.Fatalf("%d OnTurn hooks", len(as))
	}
	return as[0]
}

// capture collects the log with journald priorities.
func capture(t *testing.T) (out, errw *logx.Buffer) {
	out, errw = new(logx.Buffer), new(logx.Buffer)
	t.Cleanup(logx.SetOutput(out, errw, true))
	return out, errw
}

// With a vision model, a photo is fetched and shown to the model; history
// keeps only a note, saying the image isn't kept. A photo alone has a note,
// so it is answered. Each image is logged by size and time.
func TestPhotoShownToModel(t *testing.T) {
	out, errw := capture(t)
	var fetched []int
	h := host(t, true, &fetched)
	a := additionsOf(t, h, turn("", photo))
	if a.Note != "[1 image attached]" || a.HistoryNote != "[1 image attached"+notKept+"]" {
		t.Fatalf("got %+v", a)
	}
	if len(a.Images) != 1 || !strings.HasPrefix(a.Images[0], "data:image/png;base64,") || len(fetched) != 1 || fetched[0] != 0 {
		t.Fatalf("images %d, fetched %v", len(a.Images), fetched)
	}
	if !regexp.MustCompile(`^<6>image \d+ bytes, \d+\.\ds\n$`).MatchString(out.String()) || errw.String() != "" {
		t.Fatalf("log %q / %q", out.String(), errw.String())
	}

	// The message's own images and the quoted one's are counted together.
	a = additionsOf(t, h, turn("what's this?", photo, quoted))
	if a.Note != "[2 images attached]" || a.HistoryNote != "[2 images attached"+notKept+"]" || len(a.Images) != 2 {
		t.Fatalf("got %q, %q with %d images", a.Note, a.HistoryNote, len(a.Images))
	}
}

// Without a vision model, nothing is fetched and the model is told it can't
// see the image; history keeps the same note, and the log says so.
func TestPhotoWithoutVision(t *testing.T) {
	out, errw := capture(t)
	var fetched []int
	h := host(t, false, &fetched)
	a := additionsOf(t, h, turn("", photo))
	if !reflect.DeepEqual(a, feature.Additions{Note: "[1 image attached, which you can't see with this model]", HistoryNote: "[1 image attached, which you can't see with this model]"}) {
		t.Fatalf("got %+v", a)
	}
	a = additionsOf(t, h, turn("", photo, quoted))
	if a.Note != "[2 images attached, which you can't see with this model]" || a.Images != nil {
		t.Fatalf("got %+v", a)
	}
	if len(fetched) != 0 {
		t.Fatalf("fetched %v for a model that can't see images", fetched)
	}
	if out.String() != "<6>1 image not sent: this model can't see images\n<6>2 images not sent: this model can't see images\n" || errw.String() != "" {
		t.Fatalf("log %q / %q", out.String(), errw.String())
	}
}

// With a vision model the sticker is fetched and shown with its label;
// without one, the model is told it can't see it, and nothing is fetched.
// The log has its size, never its label.
func TestStickerShownToModel(t *testing.T) {
	out, errw := capture(t)
	var fetched []int
	h := host(t, true, &fetched)
	a := additionsOf(t, h, turn("", sticker))
	if a.Note != `[sticker attached: 🕵️ from "My Daily Life"]` || a.HistoryNote != `[sticker attached: 🕵️ from "My Daily Life"`+notKept+"]" {
		t.Fatalf("got %q, %q", a.Note, a.HistoryNote)
	}
	if len(a.Images) != 1 || !strings.HasPrefix(a.Images[0], "data:image/webp;base64,") || len(fetched) != 1 {
		t.Fatalf("images %d, fetched %v", len(a.Images), fetched)
	}
	if !regexp.MustCompile(`^<6>sticker 20 bytes, \d+\.\ds\n$`).MatchString(out.String()) {
		t.Fatalf("log %q", out.String())
	}

	fetched = nil
	h = host(t, false, &fetched)
	a = additionsOf(t, h, turn("", sticker))
	want := `[sticker attached: 🕵️ from "My Daily Life", which you can't see]`
	if !reflect.DeepEqual(a, feature.Additions{Note: want, HistoryNote: want}) || len(fetched) != 0 {
		t.Fatalf("got %+v, fetched %v", a, fetched)
	}
	notSent := "<6>sticker not sent: this model can't see images\n"
	if l := out.String(); strings.Count(l, notSent) != 1 || strings.Count(l, "\n") != 2 || strings.Contains(l, "Daily") || errw.String() != "" {
		t.Fatalf("log %q / %q", l, errw.String())
	}

	// A pack signal-cli doesn't know has no label.
	unknown := sticker
	unknown.Label = ""
	if a := additionsOf(t, h, turn("", unknown)); a.Note != "[sticker attached, which you can't see]" {
		t.Fatalf("got %q", a.Note)
	}
	if l := out.String(); strings.Count(l, notSent) != 2 || strings.Count(l, "\n") != 3 {
		t.Fatalf("log %q", l)
	}
}

// A sticker replying to a photo: the images' note, then the sticker's, and
// the sticker's picture after the photo's.
func TestPhotoAndSticker(t *testing.T) {
	var fetched []int
	h := host(t, true, &fetched)
	a := additionsOf(t, h, turn("", quoted, sticker))
	if a.Note != "[1 image attached]\n[sticker attached"+sticker.Label+"]" ||
		a.HistoryNote != "[1 image attached"+notKept+"]\n[sticker attached"+sticker.Label+notKept+"]" {
		t.Fatalf("got %q, %q", a.Note, a.HistoryNote)
	}
	if len(a.Images) != 2 || !strings.HasPrefix(a.Images[0], "data:image/png;") || !strings.HasPrefix(a.Images[1], "data:image/webp;") {
		t.Fatalf("images %d", len(a.Images))
	}
}

// One image that can't be fetched, or isn't an image, is skipped and
// logged without its content, and the model is told it failed to load, not
// that it can't see images.
func TestFetchFailures(t *testing.T) {
	var out, errw logx.Buffer
	defer logx.SetOutput(&out, &errw, true)()
	var fetched []int
	h := host(t, true, &fetched)
	data := pngBytes(t)
	h.Fetch = func(m feature.Message, i int) ([]byte, error) {
		switch i {
		case 0:
			return data, nil
		case 1:
			return nil, errors.New("timed out")
		}
		return []byte("not a picture"), nil
	}
	a := additionsOf(t, h, turn("look", photo, photo, sticker))
	if a.Note != "[1 image attached]\n[1 image attached, which failed to load; you can see images, just not this one]\n[sticker attached"+sticker.Label+", which failed to load]" ||
		a.HistoryNote != "[1 image attached"+notKept+"]\n[1 image attached, which failed to load; you can see images, just not this one]\n[sticker attached"+sticker.Label+", which failed to load]" || len(a.Images) != 1 {
		t.Fatalf("got %q with %d images", a.Note, len(a.Images))
	}
	logs := errw.String()
	if !strings.Contains(logs, "<4>could not fetch an image: timed out") ||
		!strings.Contains(logs, "<4>could not fetch a sticker: not an image") || strings.Contains(logs, "look") || strings.Contains(logs, "Alice") {
		t.Fatalf("log %q", logs)
	}
	if n := strings.Count(out.String(), "<6>image "); n != 1 || strings.Contains(out.String(), "sticker") {
		t.Fatalf("%d images logged: %q", n, out.String())
	}

	h.Fetch = nil // every fetch fails
	a = additionsOf(t, h, turn("", photo, photo))
	if a.Note != "[2 images attached, which failed to load; you can see images, just not these]" || a.Images != nil {
		t.Fatalf("got %+v", a)
	}
}

// An image too big for the model is shrunk to a JPEG that fits; the log
// has both sizes.
func TestBigPhotoIsShrunk(t *testing.T) {
	out, _ := capture(t)
	var fetched []int
	h := host(t, true, &fetched)
	m := image.NewRGBA(image.Rect(0, 0, 1160, 1160))
	x := uint32(1)
	for i := range m.Pix {
		x = x*1664525 + 1013904223
		m.Pix[i] = byte(x >> 24)
	}
	var b bytes.Buffer // noise doesn't compress, so it isn't tried: just over img.MaxBytes
	(&png.Encoder{CompressionLevel: png.NoCompression}).Encode(&b, m)
	if b.Len() <= img.MaxBytes {
		t.Fatalf("only %d bytes", b.Len())
	}
	h.Fetch = func(feature.Message, int) ([]byte, error) { return b.Bytes(), nil }
	a := additionsOf(t, h, turn("", photo))
	if a.Note != "[1 image attached]" || len(a.Images) != 1 || !strings.HasPrefix(a.Images[0], "data:image/jpeg;base64,") {
		t.Fatalf("got %q with %d images", a.Note, len(a.Images))
	}
	if !regexp.MustCompile(`^<6>image \d+ bytes, sent as \d+ bytes, \d+\.\ds\n$`).MatchString(out.String()) {
		t.Fatalf("log %q", out.String())
	}
}

// A message with no images or sticker adds nothing.
func TestTextOnly(t *testing.T) {
	var fetched []int
	h := host(t, true, &fetched)
	if a := additionsOf(t, h, turn("hello")); a.Note != "" || a.HistoryNote != "" || a.Images != nil || len(fetched) != 0 {
		t.Fatalf("got %+v", a)
	}
}

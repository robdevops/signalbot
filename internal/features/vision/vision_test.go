package vision

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"reflect"
	"strings"
	"testing"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/feature/featuretest"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

var (
	photo   = feature.Media{ID: "photo1.png", Type: "image/png"}
	quoted  = feature.Media{ID: "thumb1.jpg", Type: "image/jpeg", Quoted: true}
	sticker = feature.Media{Pack: "00aabbccddeeff00", Sticker: 2, Label: `: 🕵️ from "My Daily Life"`}
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
		if m.Media[i].Pack != "" {
			return webp, nil
		}
		return data, nil
	}
	if err := Register(h); err != nil {
		t.Fatal(err)
	}
	return h
}

func turn(text string, media ...feature.Media) feature.Turn {
	return feature.Turn{Message: feature.Message{Chat: feature.Chat{User: "u-alice"}, Sender: "u-alice", Name: "Alice", Text: text, Media: media, Addressed: true}}
}

func additionsOf(t *testing.T, h *featuretest.Host, tn feature.Turn) feature.Additions {
	t.Helper()
	as := h.Turn(context.Background(), tn)
	if len(as) != 1 {
		t.Fatalf("%d OnTurn hooks", len(as))
	}
	return as[0]
}

// With a vision model, a photo is fetched and shown to the model; history
// keeps only a note, saying the image isn't kept. A photo alone has a note,
// so it is answered.
func TestPhotoShownToModel(t *testing.T) {
	var fetched []int
	h := host(t, true, &fetched)
	a := additionsOf(t, h, turn("", photo))
	if a.Note != "[1 image attached]" || a.HistoryNote != "[1 image attached"+notKept+"]" || a.System != "" {
		t.Fatalf("got %+v", a)
	}
	if len(a.Images) != 1 || !strings.HasPrefix(a.Images[0], "data:image/png;base64,") || len(fetched) != 1 || fetched[0] != 0 {
		t.Fatalf("images %d, fetched %v", len(a.Images), fetched)
	}

	// The message's own images and the quoted one's are counted together.
	a = additionsOf(t, h, turn("what's this?", photo, quoted))
	if a.Note != "[2 images attached]" || a.HistoryNote != "[2 images attached"+notKept+"]" || len(a.Images) != 2 {
		t.Fatalf("got %q, %q with %d images", a.Note, a.HistoryNote, len(a.Images))
	}
}

// Without a vision model, nothing is fetched and the model is told it can't
// see the image; history keeps the same note.
func TestPhotoWithoutVision(t *testing.T) {
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
}

// With a vision model the sticker is fetched and shown with its label;
// without one, the model is told it can't see it, and nothing is fetched.
func TestStickerShownToModel(t *testing.T) {
	var fetched []int
	h := host(t, true, &fetched)
	a := additionsOf(t, h, turn("", sticker))
	if a.Note != `[sticker attached: 🕵️ from "My Daily Life"]` || a.HistoryNote != `[sticker attached: 🕵️ from "My Daily Life"`+notKept+"]" {
		t.Fatalf("got %q, %q", a.Note, a.HistoryNote)
	}
	if len(a.Images) != 1 || !strings.HasPrefix(a.Images[0], "data:image/webp;base64,") || len(fetched) != 1 {
		t.Fatalf("images %d, fetched %v", len(a.Images), fetched)
	}

	fetched = nil
	h = host(t, false, &fetched)
	a = additionsOf(t, h, turn("", sticker))
	want := `[sticker attached: 🕵️ from "My Daily Life", which you can't see]`
	if !reflect.DeepEqual(a, feature.Additions{Note: want, HistoryNote: want}) || len(fetched) != 0 {
		t.Fatalf("got %+v, fetched %v", a, fetched)
	}

	// A pack signal-cli doesn't know has no label.
	unknown := sticker
	unknown.Label = ""
	if a := additionsOf(t, h, turn("", unknown)); a.Note != "[sticker attached, which you can't see]" {
		t.Fatalf("got %q", a.Note)
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
// logged without its content. If none can be, the model is told it can't
// see them, as the old bot did.
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
	if a.Note != "[1 image attached]\n[sticker attached"+sticker.Label+", which you can't see]" || len(a.Images) != 1 {
		t.Fatalf("got %q with %d images", a.Note, len(a.Images))
	}
	logs := errw.String()
	if !strings.Contains(logs, "<4>could not fetch an image from Alice: timed out") ||
		!strings.Contains(logs, "<4>could not fetch a sticker from Alice: not an image") || strings.Contains(logs, "look") {
		t.Fatalf("log %q", logs)
	}

	h.Fetch = nil // every fetch fails
	a = additionsOf(t, h, turn("", photo))
	if a.Note != "[1 image attached, which you can't see with this model]" || a.Images != nil {
		t.Fatalf("got %+v", a)
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

package imagegen

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/feature/featuretest"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

func pngBytes(t *testing.T) []byte {
	t.Helper()
	m := image.NewRGBA(image.Rect(0, 0, 4, 4))
	m.Set(1, 1, color.RGBA{255, 0, 0, 255})
	var b bytes.Buffer
	if err := png.Encode(&b, m); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// host is a host whose API makes images, with settings.
func host(settings map[string]string) *featuretest.Host {
	h := featuretest.New()
	h.Bot = feature.Info{Model: "x/model", CallsTools: true, MakesImage: true}
	for k, v := range settings {
		h.Settings[k] = v
	}
	return h
}

// The tool is offered on words that ask for a picture, with GPT Image 2 at
// medium quality by default.
func TestRegister(t *testing.T) {
	var logs logx.Buffer
	defer logx.SetOutput(&logs, &logs, false)()
	h := host(nil)
	if err := Register(h); err != nil {
		t.Fatal(err)
	}
	g := h.ToolGroups()
	if len(g) != 1 || g[0].Tools[0].Name != tool || g[0].Names[0] != label {
		t.Fatalf("groups %+v", g)
	}
	if !strings.Contains(logs.String(), "images on: openai/gpt-image-2, medium quality") {
		t.Errorf("log %q", logs.String())
	}
	for _, s := range []string{"draw me a cat", "a diagram of the water cycle", "make an image of a fox", "paint a sunset",
		"pixel art of a castle", "a logo for my bakery", "sketch a house", "turn this into anime", "an illustration of a dragon", "Draw it"} {
		if !g[0].Keywords.MatchString(s) {
			t.Errorf("not offered for %q", s)
		}
	}
	for _, s := range []string{"what time is it", "start the article", "chart NVDA for a month", "a drawer of socks", "imagine that", "the departure"} {
		if g[0].Keywords.MatchString(s) {
			t.Errorf("offered for %q", s)
		}
	}
}

// It is off with IMAGE_MODEL=off, with no model, and on an API that can't
// make images; a bad IMAGE_QUALITY stops the bot.
func TestOff(t *testing.T) {
	var logs logx.Buffer
	defer logx.SetOutput(&logs, &logs, false)()
	for name, h := range map[string]*featuretest.Host{
		"off":      host(map[string]string{"IMAGE_MODEL": "off"}),
		"no model": func() *featuretest.Host { h := host(nil); h.Bot.Model = ""; return h }(),
		"other API": func() *featuretest.Host {
			h := host(nil)
			h.Bot.MakesImage = false
			return h
		}(),
	} {
		if err := Register(h); err != nil || len(h.ToolGroups()) != 0 {
			t.Errorf("%s: %v, %v", name, err, h.ToolGroups())
		}
	}
	if err := Register(host(map[string]string{"IMAGE_QUALITY": "ultra"})); err == nil {
		t.Error("IMAGE_QUALITY=ultra accepted")
	}
}

// The tool asks for the prompt as written, with a known aspect ratio, and
// returns the picture named by its title, history noting the prompt.
func TestRun(t *testing.T) {
	var logs logx.Buffer
	defer logx.SetOutput(&logs, &logs, false)()
	h := host(map[string]string{"IMAGE_MODEL": "x/painter", "IMAGE_QUALITY": "high"})
	var got feature.ImageRequest
	h.Image = func(_ context.Context, r feature.ImageRequest) ([]byte, error) {
		got = r
		return pngBytes(t), nil
	}
	Register(h)
	run := h.ToolGroups()[0].Tools[0].Run
	out, pics, err := run(context.Background(), `{"prompt":"  a fox in the snow ","aspect_ratio":"16:9","title":"Snow fox"}`)
	if err != nil || len(pics) != 1 || !strings.Contains(out, "attached") {
		t.Fatalf("%q %v %v", out, pics, err)
	}
	if got != (feature.ImageRequest{Model: "x/painter", Prompt: "a fox in the snow", AspectRatio: "16:9", Quality: "high"}) {
		t.Errorf("asked %+v", got)
	}
	if p := pics[0]; p.Name != "Snow fox" || p.Caption != "" || p.HistoryNote != "[sent a picture made from the prompt: a fox in the snow]" || !bytes.Equal(p.Data, pngBytes(t)) {
		t.Errorf("picture %+v", p)
	}
	now = func() time.Time { return time.Date(2026, 11, 19, 8, 30, 0, 0, time.Local) }
	defer func() { now = time.Now }()
	_, pics, _ = run(context.Background(), `{"prompt":"x","aspect_ratio":"7:1","title":"!!"}`)
	if got.AspectRatio != "" || pics[0].Name != "image_20261119-083000" {
		t.Errorf("ratio %q, name %q", got.AspectRatio, pics[0].Name)
	}
	for _, bad := range []string{`{"prompt":"  "}`, `not json`} {
		if _, pics, err := run(context.Background(), bad); err == nil || pics != nil {
			t.Errorf("%s: %v", bad, err)
		}
	}
}

// A failure tells the model why in a few safe words: a refusal of the
// prompt as such, anything else as the service failing.
func TestRunFails(t *testing.T) {
	h := host(nil)
	var fail error
	h.Image = func(context.Context, feature.ImageRequest) ([]byte, error) { return nil, fail }
	Register(h)
	run := h.ToolGroups()[0].Tools[0].Run
	for e, want := range map[string]string{
		"status 400: Your request was rejected by the safety system": "content policy",
		"status 402: Insufficient credits":                           "out of credit",
		"status 500: secret upstream detail":                         "the image service failed",
	} {
		fail = errors.New(e)
		out, _, err := run(context.Background(), `{"prompt":"x"}`)
		if err == nil || !strings.Contains(out, want) || strings.Contains(out, "secret") {
			t.Errorf("%s: %q", e, out)
		}
	}
	h.Image = func(context.Context, feature.ImageRequest) ([]byte, error) { return []byte("not an image"), nil }
	if out, pics, err := run(context.Background(), `{"prompt":"x"}`); err == nil || pics != nil {
		t.Errorf("not an image: %q", out)
	}
}

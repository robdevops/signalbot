package imagegen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
	h.Bot = feature.Info{Model: "x/model", CallsTools: true, Provider: "openrouter"}
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
		"pixel art of a castle", "a logo for my bakery", "sketch a house", "an illustration of a dragon", "Draw it",
		"make me a photo of a cat", "a photorealistic dog", "make a meme", "a new pfp", "a banner for my site", "make a flow chart of signup",
		"a flowchart", "an org chart for the team", "a gantt chart", "a mind map of ideas", "visualise the solar system",
		"show me a cutaway of a jet engine", "a cross-section of a vinyl groove", "a schematic of a 555 timer", "an exploded view of a gearbox", "a blueprint of my shed"} {
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
			h.Bot.Provider = ""
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
// returns the picture named by its title, history noting the prompt. Its
// log line names the chat's network.
func TestRun(t *testing.T) {
	var logs logx.Buffer
	defer logx.SetOutput(&logs, &logs, false)()
	h := host(map[string]string{"IMAGE_MODEL": "x/painter", "IMAGE_QUALITY": "high"})
	var got req
	h.API = answering(t, pngBytes(t), &got, nil)
	Register(h)
	run := h.ToolGroups()[0].Tools[0].Run
	out, pics, err := run(logx.With(context.Background(), logx.For("signal")), `{"prompt":"  a fox in the snow ","aspect_ratio":"16:9","title":"Snow fox"}`)
	if err != nil || len(pics) != 1 || !strings.Contains(out, "attached") {
		t.Fatalf("%q %v %v", out, pics, err)
	}
	if !strings.Contains(logs.String(), "Signal: image asked: no kind, 0 requirements, 16:9") || strings.Contains(logs.String(), "fox") {
		t.Errorf("log %q", logs.String())
	}
	if got != (req{Path: "/images", Model: "x/painter", Prompt: "a fox in the snow", AspectRatio: "16:9", Quality: "high"}) {
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
	h.API = answering(t, nil, nil, &fail)
	Register(h)
	run := h.ToolGroups()[0].Tools[0].Run
	for e, want := range map[string]string{
		"status 400: Your request was rejected by the safety system": "content policy",
		"status 402: Insufficient credits":                           "out of credit",
		"status 403: Your input was flagged for moderation":          "content policy",
		"status 500: secret upstream detail":                         "the image service failed",
	} {
		fail = errors.New(e)
		out, _, err := run(context.Background(), `{"prompt":"x"}`)
		if err == nil || !strings.Contains(out, want) || strings.Contains(out, "secret") {
			t.Errorf("%s: %q", e, out)
		}
	}
	fail = nil
	h.API = answering(t, []byte("not an image"), nil, &fail)
	if out, pics, err := run(context.Background(), `{"prompt":"x"}`); err == nil || pics != nil {
		t.Errorf("not an image: %q", out)
	}
}

// A technical picture's prompt goes inside a brief that puts accuracy
// first, with its requirements listed; an artistic one goes as written.
func TestTechnicalPrompt(t *testing.T) {
	h := host(nil)
	var got req
	h.API = answering(t, pngBytes(t), &got, nil)
	Register(h)
	run := h.ToolGroups()[0].Tools[0].Run
	run(context.Background(), `{"prompt":"a cutaway of a stereo record groove","kind":"technical",`+
		`"requirements":["the two walls are 90 degrees apart"," ","each wall's arrow is perpendicular to it"]}`)
	want := technicalBrief + "\n\na cutaway of a stereo record groove\n\nIt must get these exactly right:\n" +
		"- the two walls are 90 degrees apart\n- each wall's arrow is perpendicular to it\n\n" + textRules
	if got.Prompt != want {
		t.Fatalf("technical prompt:\n%s", got.Prompt)
	}
	_, pics, _ := run(context.Background(), `{"prompt":"a cutaway","kind":"technical"}`)
	if got.Prompt != technicalBrief+"\n\na cutaway\n\n"+textRules {
		t.Fatalf("with no requirements:\n%s", got.Prompt)
	}
	if pics[0].HistoryNote != "[sent a technical picture made from the prompt: a cutaway]" {
		t.Errorf("history keeps %q", pics[0].HistoryNote)
	}
	_, pics, _ = run(context.Background(), `{"prompt":"Voyager 1 at one light day","kind":"technical","requirements":["drawn to scale: distance from the Sun is linear along the axis"," ","Neptune at 30 AU"]}`)
	if want := "[sent a technical picture made from the prompt: Voyager 1 at one light day]\n[its requirements:\n" +
		"- drawn to scale: distance from the Sun is linear along the axis\n- Neptune at 30 AU]"; pics[0].HistoryNote != want {
		t.Errorf("history keeps %q", pics[0].HistoryNote)
	}
	for _, args := range []string{`{"prompt":"a fox in watercolour","kind":"artistic","requirements":["ignored"]}`, `{"prompt":"a fox in watercolour"}`} {
		run(context.Background(), args)
		if got.Prompt != "a fox in watercolour" {
			t.Fatalf("%s: %q", args, got.Prompt)
		}
	}
}

// req is what an image request asked for.
type req struct{ Path, Model, Prompt, AspectRatio, Quality string }

// answering is a fake model API that records an image request in got and
// answers it as OpenRouter's images endpoint does with data, or fails with
// *fail.
func answering(t *testing.T, data []byte, got *req, fail *error) func(context.Context, string, map[string]any) (json.RawMessage, error) {
	return func(_ context.Context, path string, body map[string]any) (json.RawMessage, error) {
		if got != nil {
			s := func(k string) string { v, _ := body[k].(string); return v }
			*got = req{path, s("model"), s("prompt"), s("aspect_ratio"), s("quality")}
			if body["n"] != 1 || body["output_format"] != "png" {
				t.Errorf("body %v", body)
			}
		}
		if fail != nil && *fail != nil {
			return nil, *fail
		}
		return json.RawMessage(fmt.Sprintf(`{"created":1,"data":[{"b64_json":%q}],"usage":{"cost":0.04}}`, base64.StdEncoding.EncodeToString(data))), nil
	}
}

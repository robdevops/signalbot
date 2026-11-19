// Package imagegen lets the model make pictures, such as drawings, art,
// illustrations, diagrams and logos, with an image model on OpenRouter.
package imagegen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/img"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

const (
	tool         = "make_image"
	defaultModel = "openai/gpt-image-2"
	label        = "**images**: drawings, art, diagrams"
)

// wantsImage matches a message that asks for a picture made, so the image
// tool is offered only then, and for the replies that follow.
var wantsImage = regexp.MustCompile(`(?i)\b(draw(s|n|ing|ings)?|drew|sketch(es|ed|ing)?|doodles?|paint(s|ed|ing|ings)?|` +
	`illustrat(e|es|ed|ing|ion|ions)|art(s|work|works|istic)?|diagrams?|images?|pictures?|pics?|logos?|icons?|posters?|` +
	`cartoons?|comics?|portraits?|wallpapers?|render(s|ed|ing)?|watercolou?rs?|anime|clip\s?art|infographics?|mock-?ups?)\b`)

// ratios are the aspect ratios the model may ask for.
var ratios = []string{"1:1", "3:2", "2:3", "16:9", "9:16", "4:3", "3:4"}

const description = "Make a picture from a text prompt with an image model, and attach it to your reply: a drawing, painting, illustration, art in any style, " +
	"a diagram, a logo, an icon, a poster or a photo-like image. Use it only when the user asks for a picture to be made, " +
	"not to describe or explain one, and not for a chart of data, which the chart tools draw from the numbers. " +
	"Write the prompt in full, as the image model sees nothing else: the subject, the style, the composition, the colours and the mood, " +
	"and any text to appear in the picture, exactly. For a diagram, name every box, label and arrow and how they connect. " +
	"After it is made, reply in one short sentence, or not at all: don't describe the picture back."

var schema = json.RawMessage(`{"type":"object","properties":{` +
	`"prompt":{"type":"string","description":"The full description of the picture for the image model."},` +
	`"aspect_ratio":{"type":"string","enum":["` + strings.Join(ratios, `","`) + `"],"description":"The picture's shape: 1:1 unless the subject suits wide (16:9, 3:2, 4:3) or tall (9:16, 2:3, 3:4)."},` +
	`"title":{"type":"string","description":"A short title for the picture, 2 to 6 words, for its file name."}},` +
	`"required":["prompt"],"additionalProperties":false}`)

// maxPrompt caps the prompt, in runes.
const maxPrompt = 4000

// now is the clock for file names; tests set it.
var now = time.Now

// Register offers the image tool when the model's API can make images and
// IMAGE_MODEL doesn't turn it off.
func Register(h feature.Host) error {
	model := strings.TrimSpace(h.Env("IMAGE_MODEL", defaultModel))
	quality := strings.ToLower(strings.TrimSpace(h.Env("IMAGE_QUALITY", "medium")))
	info := h.Info()
	switch {
	case !txt.Bool(model, true) || model == "":
		logx.Infof("images: off (IMAGE_MODEL)")
		return nil
	case info.Model == "":
		return nil
	case !info.MakesImage:
		logx.Infof("images: off, as only OpenRouter's API makes them")
		return nil
	case !slices.Contains([]string{"low", "medium", "high", "auto"}, quality):
		return fmt.Errorf("IMAGE_QUALITY is %q: use low, medium, high or auto", quality)
	}
	if h.OfferTools(feature.ToolGroup{
		Names:    []string{label},
		Listing:  "images (" + tool + ")",
		Keywords: wantsImage,
		Tools:    []feature.Tool{{Name: tool, Description: description, Schema: schema, Run: run(h, model, quality)}},
	}) {
		logx.Noticef("images on: %s, %s quality", model, quality)
	}
	return nil
}

type args struct {
	Prompt      string `json:"prompt"`
	AspectRatio string `json:"aspect_ratio"`
	Title       string `json:"title"`
}

var errArgs = errors.New("bad arguments")

func run(h feature.Host, model, quality string) func(context.Context, string) (string, []feature.Picture, error) {
	return func(ctx context.Context, raw string) (string, []feature.Picture, error) {
		var a args
		if err := json.Unmarshal([]byte(raw), &a); err != nil {
			return "error: the arguments are not valid JSON", nil, errArgs
		}
		a.Prompt = strings.TrimSpace(a.Prompt)
		if a.Prompt == "" {
			return "error: prompt is empty", nil, errArgs
		}
		if !slices.Contains(ratios, a.AspectRatio) {
			a.AspectRatio = ""
		}
		a.Prompt = txt.Clamp(a.Prompt, maxPrompt)
		data, err := h.GenerateImage(ctx, feature.ImageRequest{Model: model, Prompt: a.Prompt, AspectRatio: a.AspectRatio, Quality: quality})
		if err != nil {
			return "error: the image could not be made (" + reason(err) + "). Tell the user, briefly.", nil, err
		}
		if data, err = img.Fit(data); err != nil {
			return "error: the image service returned something that can't be sent", nil, err
		}
		title := txt.Clamp(txt.SafeName(a.Title), 60)
		return "The picture is made and attached to your reply.", []feature.Picture{{Data: data, Name: fileName(title),
			HistoryNote: "[sent a picture made from the prompt: " + txt.Clamp(a.Prompt, 400) + "]"}}, nil
	}
}

// reason is a short, safe reason a request failed, for the model: the
// API's own words only for a refusal of the prompt, which the user should
// hear about.
func reason(err error) string {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "status 400") && (strings.Contains(s, "safety") || strings.Contains(s, "moderat") || strings.Contains(s, "policy")):
		return "the image service refused the prompt under its content policy"
	case strings.Contains(s, "status 402"):
		return "the account is out of credit"
	case strings.Contains(s, "status 429"):
		return "too many requests just now"
	case errors.Is(err, context.DeadlineExceeded):
		return "it took too long"
	}
	return "the image service failed"
}

// fileName is the picture's title, or "image_" and the local time if that
// has no letter or digit.
func fileName(title string) string {
	if strings.IndexFunc(title, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0 {
		return title
	}
	return "image_" + now().Format("20060102-150405")
}

// Package charts lets the model draw line and pie charts, sent to the chat as
// images captioned with their titles in place of the model's text.
package charts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// wantsChart matches a message that asks for a chart, or for a projection,
// forecast or trend a line chart shows well, so the chart tools are offered
// only then, and for the replies that follow.
var wantsChart = regexp.MustCompile(`(?i)\b(chart(s|ed|ing)?|graph(s|ed|ing)?|plot(s|ted|ting)?|visuali[sz](e|es|ed|ing|ation|ations)|pies?|donuts?|doughnuts?|` +
	`projections?|forward\s+estimates?|trending|forecast(s|ed|ing)?)\b`)

// errArgs marks arguments the model must fix. The log gets the same reason
// the model does, which can quote a series name or a date from its
// arguments, but never the user's message.
var errArgs = errors.New("invalid arguments")

// now is the clock DARKMODE=auto reads; tests replace it.
var now = time.Now

// darkMode reads DARKMODE: "off" (the default), "on" or "auto". Any other
// value is taken as off, with a warning.
func darkMode(h feature.Host) string {
	v := h.Env("DARKMODE", "off")
	switch m := strings.ToLower(v); m {
	case "off", "on", "auto":
		return m
	}
	logx.Warnf("DARKMODE=%q is not off, on or auto; charts are light", txt.Clamp(v, 20))
	return "off"
}

// themeFor is the theme a chart is drawn in now: dark when mode is "on", or
// "auto" from 21:00 to 07:00 local time (TZ); otherwise light.
func themeFor(mode string) *theme {
	if hour := now().In(time.Local).Hour(); mode == "on" || mode == "auto" && (hour >= 21 || hour < 7) {
		return darkTheme
	}
	return lightTheme
}

// Register offers the chart tools on h, and sends a chart alone with its
// caption.
func Register(h feature.Host) error {
	mode := darkMode(h)
	h.Tools(feature.ToolGroup{
		Names:    []string{"**charts**: line, pie"},
		Listing:  "charts (" + chartTool + ", " + pieTool + ")",
		Keywords: wantsChart,
		Tools: []feature.Tool{
			{Name: chartTool, Description: chartToolDescription, Schema: chartToolSchema, Run: func(_ context.Context, args string) (string, []feature.Picture, error) {
				s, err := parseChart(args)
				if err != nil {
					return "error: " + err.Error(), nil, fmt.Errorf("%w: %v", errArgs, err)
				}
				data, err := renderChart(s, themeFor(mode))
				return drawn(s.summary(), chartCaption(s.Title, s.Subtitle), fileName("line", s.Title), data, err)
			}},
			{Name: pieTool, Description: pieToolDescription, Schema: pieToolSchema, Run: func(_ context.Context, args string) (string, []feature.Picture, error) {
				s, err := parsePie(args)
				if err != nil {
					return "error: " + err.Error(), nil, fmt.Errorf("%w: %v", errArgs, err)
				}
				data, err := renderPie(s, themeFor(mode))
				return drawn(s.summary(), chartCaption(s.Title, s.Subtitle), fileName("pie", s.Title), data, err)
			}},
		},
	})
	return nil
}

// drawn is a chart tool's result: the summary for the model and the chart,
// or what went wrong drawing it.
func drawn(summary, caption, name string, data []byte, err error) (string, []feature.Picture, error) {
	if err != nil {
		return "error: the chart could not be drawn", nil, err
	}
	return summary, []feature.Picture{{Data: data, Name: name, Caption: caption, HistoryNote: sentNote(caption)}}, nil
}

// fileName is what a chart's file is called: its title, or, if that has no
// letter or digit, its kind and the local time, e.g. "pie_20261101-223115".
func fileName(kind, title string) string {
	if strings.IndexFunc(title, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0 {
		return title
	}
	return kind + "_" + now().Format("20060102-150405")
}

// sentNote is what history keeps of a chart, which the model would
// otherwise not know it sent, e.g. `[sent the chart "NVDA"]`.
func sentNote(caption string) string {
	return "[sent the chart " + strconv.Quote(caption) + "]"
}

// chartNote opens a chart tool's result for the model.
const chartNote = "Chart attached. It is sent captioned with its title in place of your text, which won't be shown, so reply with just \"Done\". "

// captionMax is about the characters that fit on one line under a chart in
// Signal on a phone.
const captionMax = 36

// chartCaption is the text sent with a chart: "title — subtitle" if that fits
// on one line, else the title.
func chartCaption(title, subtitle string) string {
	if c := title + " — " + subtitle; subtitle != "" && len([]rune(c)) <= captionMax {
		return c
	}
	return title
}

// decodeArgs decodes a tool's arguments into v, with an error that tells the
// model what to fix. Empty arguments are taken as {}, and arguments sent as a
// JSON string holding the JSON are unwrapped.
func decodeArgs(tool, arguments string, v any) error {
	data := []byte(strings.TrimSpace(arguments))
	if len(data) == 0 {
		data = []byte("{}")
	}
	var inner string
	if data[0] == '"' && json.Unmarshal(data, &inner) == nil {
		data = []byte(inner)
	}
	var typeErr *json.UnmarshalTypeError
	var syntaxErr *json.SyntaxError
	switch err := json.Unmarshal(data, v); {
	case err == nil:
		return nil
	case errors.As(err, &typeErr) && typeErr.Field != "":
		return fmt.Errorf("%s must be %s, got %s", typeErr.Field, jsonType(typeErr.Type.Kind()), typeErr.Value)
	case errors.As(err, &typeErr):
		return fmt.Errorf("the arguments for %s must be a JSON object, got %s", tool, typeErr.Value)
	case errors.As(err, &syntaxErr):
		return fmt.Errorf("the arguments for %s are not valid JSON (at character %d): %v", tool, syntaxErr.Offset, err)
	default:
		return fmt.Errorf("the arguments for %s are not valid JSON: %v", tool, err)
	}
}

// jsonType names a Go kind as JSON calls it, e.g. "a number".
func jsonType(k reflect.Kind) string {
	switch k {
	case reflect.String:
		return "a string"
	case reflect.Bool:
		return "true or false"
	case reflect.Float64, reflect.Float32, reflect.Int, reflect.Int64:
		return "a number"
	case reflect.Slice, reflect.Array:
		return "an array"
	default:
		return "an object"
	}
}

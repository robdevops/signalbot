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

// wantsSecondAxis matches a message that asks for a second Y axis, so the
// dual-axis chart tool is offered only then, and for the replies that follow.
var wantsSecondAxis = regexp.MustCompile(`(?i)\b(second(ary)?|dual|double|two|2|twin|both|separate|another|extra|right([\s-]*hand)?)[\s-]+(y[\s-]*)?ax(is|es)\b`)

// errArgs marks arguments the model must fix. The log gets the same reason
// the model does, which can quote a series name or a date from its
// arguments, but never the user's message.
var errArgs = errors.New("invalid arguments")

// now is the clock DARKMODE reads; tests replace it.
var now = time.Now

// darkHours is when charts are dark: from one minute of the day up to
// another, local time (TZ), wrapping midnight when the end is earlier.
type darkHours struct{ from, to int }

var (
	never  = darkHours{}
	allDay = darkHours{0, 24 * 60}
)

// clockPattern is a time of day: 18, 18:30, 6pm, 6:30 am.
var clockPattern = regexp.MustCompile(`^(\d{1,2})(?::(\d{2}))?\s*(am|pm)?$`)

// darkMode reads DARKMODE: off (the default), on, or the hours charts are
// dark, such as 18:00-6:00, 18-6 or 6pm-6am. Any other value is taken as
// off, with a warning.
func darkMode(h feature.Host) darkHours {
	v := h.Env("DARKMODE", "off")
	switch m := strings.ToLower(strings.TrimSpace(v)); m {
	case "off":
		return never
	case "on":
		return allDay
	default:
		if from, to, ok := strings.Cut(m, "-"); ok {
			if f, ok := clock(from); ok {
				if t, ok := clock(to); ok && f != t {
					return darkHours{f, t}
				}
			}
		}
	}
	logx.Warnf("DARKMODE=%q is not off, on or a range of times such as 18:00-6:00; charts are light", txt.Clamp(v, 20))
	return never
}

// clock is a time of day as minutes since midnight: 18, 18:30, 6pm, 6:30am,
// 12am (midnight) or 12pm (noon).
func clock(s string) (int, bool) {
	m := clockPattern.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	h, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	switch {
	case min > 59, m[3] == "" && h > 23, m[3] != "" && (h < 1 || h > 12):
		return 0, false
	case m[3] == "am" && h == 12:
		h = 0
	case m[3] == "pm" && h < 12:
		h += 12
	}
	return h*60 + min, true
}

// dark reports whether charts are dark at t, by the local time (TZ).
func (d darkHours) dark(t time.Time) bool {
	t = t.In(time.Local)
	m := t.Hour()*60 + t.Minute()
	if d.from <= d.to {
		return m >= d.from && m < d.to
	}
	return m >= d.from || m < d.to
}

// themeFor is the theme a chart is drawn in now, decided as each is drawn.
func themeFor(d darkHours) *theme {
	if d.dark(now()) {
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
			{Name: chartTool, Description: chartToolDescription, Schema: chartToolSchema, Run: lineChart(mode, false)},
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
	// No Names: it is part of the charts the intro lists.
	h.Tools(feature.ToolGroup{
		Listing:  "dual-axis chart (" + dualAxisTool + ")",
		Keywords: wantsSecondAxis,
		Tools:    []feature.Tool{{Name: dualAxisTool, Description: dualAxisToolDescription, Schema: dualAxisToolSchema, Run: lineChart(mode, true)}},
	})
	return nil
}

// lineChart runs a line chart tool, on two Y axes if dual.
func lineChart(mode darkHours, dual bool) func(context.Context, string) (string, []feature.Picture, error) {
	return func(_ context.Context, args string) (string, []feature.Picture, error) {
		s, err := parseChart(args)
		if err == nil && !dual && s.right.used {
			err = errors.New(chartTool + " has one Y axis: put every series on it, using change mode to compare series on different scales")
		}
		if err != nil {
			return "error: " + err.Error(), nil, fmt.Errorf("%w: %v", errArgs, err)
		}
		data, err := renderChart(s, themeFor(mode))
		return drawn(s.summary(), chartCaption(s.Title, s.Subtitle), fileName("line", s.Title), data, err)
	}
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
	err := json.Unmarshal(data, v)
	// Some models pass an object or array as a string of its JSON, e.g.
	// "axes": "{\"left\": …}": take each such field, as the error names it,
	// as meant, and try again.
	var tree any
	for range 20 {
		if !errors.As(err, &typeErr) || typeErr.Value != "string" || typeErr.Field == "" ||
			(tree == nil && json.Unmarshal(data, &tree) != nil) || !unstring(tree, strings.Split(typeErr.Field, ".")) {
			break
		}
		fixed, merr := json.Marshal(tree)
		if merr != nil {
			break
		}
		err = json.Unmarshal(fixed, v)
	}
	switch {
	case err == nil:
		return nil
	case errors.As(err, &typeErr) && typeErr.Field != "" && typeErr.Value == "string":
		return fmt.Errorf("%s must be %s, got a string: pass it as JSON itself, not as text in quotes", typeErr.Field, jsonType(typeErr.Type.Kind()))
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

// unstring replaces the string at path (keys and indices) in a decoded JSON
// value, if it holds a JSON object or array, with what it holds, and
// reports whether it did.
func unstring(v any, path []string) bool {
	for len(path) > 1 {
		switch t := v.(type) {
		case map[string]any:
			v = t[path[0]]
		case []any:
			i, err := strconv.Atoi(path[0])
			if err != nil || i < 0 || i >= len(t) {
				return false
			}
			v = t[i]
		default:
			return false
		}
		path = path[1:]
	}
	if len(path) == 0 {
		return false
	}
	parse := func(e any) (any, bool) {
		str, ok := e.(string)
		if s := strings.TrimSpace(str); !ok || s == "" || s[0] != '{' && s[0] != '[' {
			return nil, false
		}
		var inner any
		return inner, json.Unmarshal([]byte(str), &inner) == nil
	}
	switch t := v.(type) {
	case map[string]any: // a field of an object
		if inner, ok := parse(t[path[0]]); ok {
			t[path[0]] = inner
			return true
		}
	case []any: // an element of an array
		if i, err := strconv.Atoi(path[0]); err == nil && i >= 0 && i < len(t) {
			if inner, ok := parse(t[i]); ok {
				t[i] = inner
				return true
			}
		}
	}
	return false
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

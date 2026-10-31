// Package charts gives the model two chart tools, line and donut, offered
// when a message asks for a chart and for the next 2 replies in that chat.
// A reply with a chart is sent as one message: the chart, captioned with
// its title in place of the model's text.
package charts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/bound"
)

// Feature is the chart tools. Its state is the follow-ups left in each chat.
type Feature struct {
	mu     sync.Mutex
	charts map[chat.ChatID]int // replies left with the chart tools on
}

func (*Feature) Name() string { return "charts" }

func (f *Feature) Register(h feature.Host) error {
	f.charts = map[chat.ChatID]int{}
	h.Tools(feature.ToolGroup{Prompt: "charts (" + chartTool + ", " + donutTool + ")", Intro: "charts", Tools: Tools(), Offer: f.offer})
	h.OnReply(f.replied)
	h.OnForget(f.forget)
	return nil
}

func functionDef(name, description string, schema json.RawMessage) map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{
		"name": name, "description": description, "parameters": schema,
	}}
}

// Tools are the line and donut chart tools.
func Tools() []feature.Tool {
	return []feature.Tool{
		{Name: chartTool, Label: chartTool, Def: functionDef(chartTool, chartToolDescription, chartToolSchema),
			Run: func(_ context.Context, args string) (feature.Result, error) {
				s, err := parseChart(args)
				if err != nil {
					return feature.Result{Text: "error: " + err.Error()}, err
				}
				img, err := renderLine(s)
				if err != nil {
					return feature.Result{Text: "error: the chart could not be drawn"}, err
				}
				return feature.Result{Text: chartNote + s.summary(), Images: []chat.Picture{{Data: img, Caption: chartCaption(s.Title, s.Subtitle)}}}, nil
			}},
		{Name: donutTool, Label: donutTool, Def: functionDef(donutTool, donutToolDescription, donutToolSchema),
			Run: func(_ context.Context, args string) (feature.Result, error) {
				s, err := parseDonut(args)
				if err != nil {
					return feature.Result{Text: "error: " + err.Error()}, err
				}
				img, err := renderDonut(s)
				if err != nil {
					return feature.Result{Text: "error: the chart could not be drawn"}, err
				}
				return feature.Result{Text: chartNote + s.summary(), Images: []chat.Picture{{Data: img, Caption: chartCaption(s.Title, s.Subtitle)}}}, nil
			}},
	}
}

// followUps is how many replies after one that asked for or drew a chart
// still offer the chart tools, for follow-ups like "show that for a month".
const followUps = 2

// maxChats bounds the chats counted: chat ids come from the network.
const maxChats = 500

// wantsChart reports whether a message asks for a chart, so the chart tools
// are offered only then.
var wantsChart = regexp.MustCompile(`(?i)\b(chart(s|ed|ing)?|graph(s|ed|ing)?|plot(s|ted|ting)?|visuali[sz](e|es|ed|ing|ation|ations)|pies?|donuts?|doughnuts?)\b`).MatchString

// offer is on when the message asks for a chart, or a recent one did; never
// on a chat command's turn, which offers only its own tool.
func (f *Feature) offer(t feature.Turn) bool {
	return t.Forced == "" && (wantsChart(t.Message.Text) || f.turns(t.Message.Chat) > 0)
}

func (f *Feature) turns(c chat.ChatID) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.charts[c]
}

// replied counts a reply that offered the chart tools: one that asked for
// or drew a chart restarts the count, any other uses one up. A reply with
// charts is sent as their captions.
func (f *Feature) replied(t feature.Turn, r feature.Reply) *chat.Outgoing {
	if slices.Contains(t.Offered, chartTool) {
		drew := slices.ContainsFunc(r.Used, func(n string) bool { return n == chartTool || n == donutTool })
		f.count(t.Message.Chat, wantsChart(t.Message.Text) || drew)
	}
	if c := captions(r.Pictures); c != "" {
		return &chat.Outgoing{Chat: t.Message.Chat, Text: c}
	}
	return nil
}

func (f *Feature) count(c chat.ChatID, fresh bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case fresh:
		bound.MakeRoom(f.charts, c, maxChats)
		f.charts[c] = followUps
	case f.charts[c] > 1:
		f.charts[c]--
	default:
		delete(f.charts, c)
	}
}

// forget drops a chat's count with its history: nothing left to follow up on.
func (f *Feature) forget(c chat.ChatID) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.charts, c)
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

// captions is the caption for a reply's charts, one per line, or "" if it
// has none.
func captions(pics []chat.Picture) string {
	var caps []string
	for _, p := range pics {
		if p.Caption != "" {
			caps = append(caps, p.Caption)
		}
	}
	return strings.Join(caps, "\n")
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

// chartTool is the built-in tool the model calls to draw a line chart, which
// is sent to the chat as an image.
const chartTool = "make_chart"

var chartToolSchema = json.RawMessage(`{
  "type": "object",
  "required": ["title", "series"],
  "properties": {
    "title": {"type": "string", "description": "Short title ending with the period in words, e.g. \"NVDA vs AMD, 1 month\" or \"Sydney rainfall, 5 years\"; it is also the message caption"},
    "subtitle": {"type": "string", "description": "e.g. \"% change over the past month, 30 Sep - 28 Oct 2026\""},
    "axes": {"type": "object", "description": "Settings for the left and right Y axes. Put series with a different unit or scale on the right axis. A chart that uses both axes must give each a title.",
      "properties": {"left": {"$ref": "#/$defs/axis"}, "right": {"$ref": "#/$defs/axis"}}},
    "series": {"type": "array", "minItems": 1, "maxItems": 6, "items": {
      "type": "object", "required": ["name", "points"],
      "properties": {
        "name": {"type": "string", "description": "Legend label, e.g. \"NVDA\" or \"Sydney\""},
        "axis": {"type": "string", "enum": ["left", "right"], "description": "Which Y axis the series uses (default left)"},
        "points": {"type": "array", "minItems": 2, "maxItems": 1000, "items": {
          "type": "object", "required": ["date", "value"],
          "properties": {"date": {"type": "string", "description": "YYYY-MM-DD"}, "value": {"type": "number"}}
        }}
      }
    }}
  },
  "$defs": {"axis": {"type": "object", "properties": {
    "title": {"type": "string", "description": "Axis title, e.g. \"Price ($)\" or \"Rainfall (mm)\"; required on both axes when both are used"},
    "mode": {"type": "string", "enum": ["change", "value"], "description": "change (default): plot each series as % change from its first point. value: plot the values as given"},
    "unit": {"type": "string", "description": "Prefix for values in value mode, e.g. \"$\""},
    "invert": {"type": "boolean", "description": "Draw this axis upside down: higher values lower"}
  }}}
}`)

const chartToolDescription = "Draw a line chart of one or more series over time, on one or two Y axes, and attach it to your reply as an image. " +
	"Use it when a chart would help, e.g. to show a trend or compare series. Pass the raw values: in change mode the % change is computed for you. " +
	"The chart is sent alone with its title as the caption, so write no other text: it would be discarded."

// parseChart decodes and checks the model's arguments for make_chart.
func parseChart(arguments string) (*lineSpec, error) {
	var s lineSpec
	if err := decodeArgs(chartTool, arguments, &s); err != nil {
		return nil, err
	}
	return &s, s.validate()
}

// donutTool is the built-in tool the model calls to draw a donut chart of
// shares of a total, sent to the chat as an image.
const donutTool = "make_donut_chart"

var donutToolSchema = json.RawMessage(`{
  "type": "object",
  "required": ["title", "slices"],
  "properties": {
    "title": {"type": "string", "description": "Short title, e.g. \"Revenue by segment, Q3\""},
    "subtitle": {"type": "string", "description": "e.g. \"Share of total\""},
    "slices": {"type": "array", "minItems": 2, "maxItems": 12, "items": {
      "type": "object", "required": ["label", "value"],
      "properties": {"label": {"type": "string"}, "value": {"type": "number", "minimum": 0}}
    }},
    "unit": {"type": "string", "description": "Prefix for the total in the centre, e.g. \"$\""},
    "suffix": {"type": "string", "description": "The values' scale, K, M, B or T (e.g. \"B\" if 54.3 means 54.3 billion), or a unit such as \"kg\". Large totals are shortened for you"},
    "center": {"type": "string", "description": "Text for the centre instead of the total"}
  }
}`)

const donutToolDescription = "Draw a donut chart of how a total splits into parts and attach it to your reply as an image. " +
	"Use it for proportions. Pass the raw values: the percentages and total are computed for you. " +
	"The chart is sent alone with its title as the caption, so write no other text: it would be discarded."

// parseDonut decodes and checks the model's arguments for make_donut_chart.
func parseDonut(arguments string) (*donutSpec, error) {
	var s donutSpec
	if err := decodeArgs(donutTool, arguments, &s); err != nil {
		return nil, err
	}
	return &s, s.validate()
}

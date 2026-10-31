package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/robdevops/signalbot/internal/kit/linechart"
	"reflect"
	"strings"
)

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

// parseChart decodes and checks the model's arguments for make_chart.
func parseChart(arguments string) (*linechart.Spec, error) {
	var s linechart.Spec
	if err := decodeArgs(chartTool, arguments, &s); err != nil {
		return nil, err
	}
	return &s, s.Validate()
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

package charts

import (
	"encoding/json"
	"github.com/robdevops/signalbot/internal/kit/linechart"
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

// parseChart decodes and checks the model's arguments for make_chart.
func parseChart(arguments string) (*linechart.Spec, error) {
	var s linechart.Spec
	if err := decodeArgs(chartTool, arguments, &s); err != nil {
		return nil, err
	}
	return &s, s.Validate()
}

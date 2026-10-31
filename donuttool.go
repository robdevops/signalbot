package main

import (
	"encoding/json"
	"github.com/robdevops/signalbot/internal/kit/donut"
)

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
func parseDonut(arguments string) (*donut.Spec, error) {
	var s donut.Spec
	if err := decodeArgs(donutTool, arguments, &s); err != nil {
		return nil, err
	}
	return &s, s.Validate()
}

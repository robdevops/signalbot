// Package charts gives the model two chart tools, line and donut, offered
// when a message asks for a chart and for the next 2 replies in that chat.
// A reply with a chart is sent as one message: the chart, captioned with
// its title in place of the model's text.
package charts

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/donut"
	"github.com/robdevops/signalbot/internal/kit/linechart"
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
				img, err := linechart.Render(s)
				if err != nil {
					return feature.Result{Text: "error: the chart could not be drawn"}, err
				}
				return feature.Result{Text: chartNote + s.Summary(), Images: []chat.Picture{{Data: img, Caption: chartCaption(s.Title, s.Subtitle)}}}, nil
			}},
		{Name: donutTool, Label: donutTool, Def: functionDef(donutTool, donutToolDescription, donutToolSchema),
			Run: func(_ context.Context, args string) (feature.Result, error) {
				s, err := parseDonut(args)
				if err != nil {
					return feature.Result{Text: "error: " + err.Error()}, err
				}
				img, err := donut.Render(s)
				if err != nil {
					return feature.Result{Text: "error: the chart could not be drawn"}, err
				}
				return feature.Result{Text: chartNote + s.Summary(), Images: []chat.Picture{{Data: img, Caption: chartCaption(s.Title, s.Subtitle)}}}, nil
			}},
	}
}

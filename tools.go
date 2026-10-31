package main

import (
	"cmp"
	"context"
	"encoding/json"
	"slices"

	"github.com/robdevops/signalbot/internal/llm"
)

// webSearch is OpenRouter's server tool: the model calls it only when it
// decides to search, unlike the deprecated "web" plugin, which searched on
// every request.
var webSearch = llm.Tool{Name: "web_search", Label: "web search", Def: map[string]string{"type": "openrouter:web_search"}}

// tools is what the model is offered for a turn: web search, the chart tools
// when the turn wants a chart, and the MCP tools; or only a chat command's
// tool. None if the model can't call tools.
func (b *Bot) tools(chart bool, force string) []llm.Tool {
	if !b.toolCalls {
		return nil
	}
	if force != "" {
		if t, ok := b.mcp.tool(force); ok {
			return []llm.Tool{t}
		}
		return nil
	}
	var out []llm.Tool
	if b.cfg.Search && b.serverTools {
		out = append(out, webSearch)
	}
	if chart {
		out = append(out, chartTools()...)
	}
	return append(out, b.mcp.llmTools()...)
}

// charted reports whether a reply drew a chart.
func charted(c llm.Completion) bool {
	return slices.ContainsFunc(c.Used, func(n string) bool { return n == chartTool || n == donutTool })
}

func functionDef(name, description string, schema json.RawMessage) map[string]any {
	return map[string]any{"type": "function", "function": map[string]any{
		"name": name, "description": description, "parameters": schema,
	}}
}

// chartTools are the built-in line and donut chart tools.
func chartTools() []llm.Tool {
	return []llm.Tool{
		{Name: chartTool, Label: chartTool, Def: functionDef(chartTool, chartToolDescription, chartToolSchema),
			Run: func(_ context.Context, args string) (llm.Result, error) {
				s, err := parseChart(args)
				if err != nil {
					return llm.Result{Text: "error: " + err.Error()}, err
				}
				img, err := renderChart(s)
				if err != nil {
					return llm.Result{Text: "error: the chart could not be drawn"}, err
				}
				return llm.Result{Text: s.summary(), Images: []llm.Image{{Data: img, Caption: chartCaption(s.Title, s.Subtitle)}}}, nil
			}},
		{Name: donutTool, Label: donutTool, Def: functionDef(donutTool, donutToolDescription, donutToolSchema),
			Run: func(_ context.Context, args string) (llm.Result, error) {
				s, err := parseDonut(args)
				if err != nil {
					return llm.Result{Text: "error: " + err.Error()}, err
				}
				img, err := renderDonut(s)
				if err != nil {
					return llm.Result{Text: "error: the chart could not be drawn"}, err
				}
				return llm.Result{Text: s.summary(), Images: []llm.Image{{Data: img, Caption: chartCaption(s.Title, s.Subtitle)}}}, nil
			}},
	}
}

// llmTools are the MCP tools as the model is offered them.
func (m *MCP) llmTools() []llm.Tool {
	if m == nil {
		return nil
	}
	out := make([]llm.Tool, len(m.tools))
	for i, t := range m.tools {
		out[i] = m.llmTool(t)
	}
	return out
}

// tool is one MCP tool by the model's name for it.
func (m *MCP) tool(model string) (llm.Tool, bool) {
	if m == nil {
		return llm.Tool{}, false
	}
	t, ok := m.byModel[model]
	return m.llmTool(t), ok
}

func (m *MCP) llmTool(t mcpTool) llm.Tool {
	schema := t.Schema
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type":"object"}`)
	}
	return llm.Tool{Name: t.Model, Label: cmp.Or(t.Name, "unknown") + " (" + cmp.Or(t.Server, "no server") + ")",
		Def: functionDef(t.Model, t.Description, schema),
		Run: func(ctx context.Context, args string) (llm.Result, error) {
			out, images, _, err := m.Call(ctx, t.Model, args)
			res := llm.Result{Text: out}
			for _, img := range images {
				res.Images = append(res.Images, llm.Image{Data: img})
			}
			return res, err
		}}
}

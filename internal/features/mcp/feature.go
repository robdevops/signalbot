// Package mcp offers the model the tools of the MCP servers in MCP_CONFIG,
// and turns chosen tools into chat commands: "/name text" is a turn in
// which the model must call that tool.
package mcp

import (
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"

	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

// Feature is the MCP servers. Version is sent as the client's version;
// Model names the model in a warning.
type Feature struct {
	Version string
	Model   string
	m       *MCP
}

func (*Feature) Name() string { return "mcp" }

func (f *Feature) Register(h feature.Host) error {
	path := h.Env().String("MCP_CONFIG", "")
	if path == "" {
		return nil
	}
	servers, err := loadMCPConfig(path)
	if err == nil {
		f.m, err = StartMCP(h.Context(), servers, f.Version)
	}
	if err != nil {
		return err
	}
	if names := f.m.Names(); len(names) > 0 {
		h.Tools(feature.ToolGroup{Prompt: "MCP tools by server, " + f.m.ByServer(), Intro: strings.Join(names, ", "),
			Grouped: true, Tools: f.m.tools()})
		logx.Infof("MCP tools: %s", strings.Join(names, ", "))
	}
	if !h.Caps().Tools {
		if len(f.m.cmds) > 0 {
			logx.Warnf("model %s can't call tools, so chat commands are off", f.Model)
		}
		return nil
	}
	var cmds []string
	for _, name := range slices.Sorted(maps.Keys(f.m.cmds)) {
		c := f.m.cmds[name]
		if h.Command(name, command(name, c)) != nil {
			logx.Warnf("MCP server %s: chat command %q is not a free name of letters, digits, _ or -; skipped", c.Server, name)
			continue
		}
		cmds = append(cmds, "/"+name)
	}
	if len(cmds) > 0 {
		h.Tools(feature.ToolGroup{Prompt: "chat commands " + strings.Join(cmds, ", "), Intro: strings.Join(cmds, ", ")})
		logx.Infof("chat commands: %s", strings.Join(cmds, ", "))
	}
	return nil
}

// Wait waits for local servers to stop once the bot's context ends.
func (f *Feature) Wait() { f.m.Wait() }

// command handles "/name rest": with require_arg, a bare command is
// answered by the bot; otherwise the model must call the tool.
func command(name string, c mcpCommand) func(context.Context, feature.Turn, string) (feature.Command, bool) {
	return func(_ context.Context, t feature.Turn, rest string) (feature.Command, bool) {
		if c.RequireArg && rest == "" {
			return feature.Command{Text: "/" + name + " needs something after it."}, true
		}
		logx.Infof("chat command /%s from %s", name, cmp.Or(t.Message.Sender.Name, t.Message.Sender.ID))
		return feature.Command{Tool: c.Tool}, true
	}
}

// tools are the servers' tools as the model is offered them.
func (m *MCP) tools() []feature.Tool {
	out := make([]feature.Tool, len(m.list))
	for i, t := range m.list {
		schema := t.Schema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		out[i] = feature.Tool{Name: t.Model, Label: cmp.Or(t.Name, "unknown") + " (" + cmp.Or(t.Server, "no server") + ")",
			Def: map[string]any{"type": "function", "function": map[string]any{"name": t.Model, "description": t.Description, "parameters": schema}},
			Run: func(ctx context.Context, args string) (feature.Result, error) {
				out, images, _, err := m.Call(ctx, t.Model, args)
				res := feature.Result{Text: out}
				for _, img := range images {
					res.Images = append(res.Images, chat.Picture{Data: img})
				}
				return res, err
			}}
	}
	return out
}

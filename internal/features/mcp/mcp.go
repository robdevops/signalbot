// Package mcp offers the model the tools of the MCP servers in MCP_CONFIG,
// one tool group per server, and turns them into chat commands.
package mcp

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/httpx"
	"github.com/robdevops/signalbot/internal/kit/img"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

const (
	protocol    = "2025-06-18"
	callTimeout = 30 * time.Second
	// maxResult caps what one tool call adds to the conversation, in runes.
	maxResult = 16 << 10
)

// server is one entry of MCP_CONFIG's "mcpServers": a URL for a remote
// server, or a command for a local one. allowed_tools, when present, offers
// the model only those tools; blocked_tools, when present, offers all but
// those. Without either, every tool the server lists is offered.
type server struct {
	URL          string                 `json:"url"`
	Headers      map[string]string      `json:"headers"`
	Command      string                 `json:"command"`
	Args         []string               `json:"args"`
	Env          map[string]string      `json:"env"`
	AllowedTools []string               `json:"allowed_tools"` // nil: not in whitelist mode
	BlockedTools []string               `json:"blocked_tools"`
	Disabled     bool                   `json:"disabled"` // left out entirely
	ChatCommands map[string]chatCommand `json:"chat_commands"`
	Instructions *string                `json:"instructions"` // a system-prompt line while its tools are offered; unset: the server's own, "": none
	Keywords     []string               `json:"keywords"`     // if set, its tools are offered only when a message has one of these words
}

// chatCommand makes "/name ..." a turn in which the model must call tool.
// With require_arg, a bare "/name" is answered by the bot, not the model.
type chatCommand struct {
	Tool       string `json:"tool"`
	RequireArg bool   `json:"require_arg"`
}

var unsafeToolChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// Register starts the servers in MCP_CONFIG, if set, and offers their tools
// on h. They run as long as the bot: when it stops, local servers get
// SIGTERM and the bot waits for them. With no model, none is started.
func Register(h feature.Host) error {
	path := h.Env("MCP_CONFIG", "")
	if path == "" {
		return nil
	}
	if h.Info().Model == "" {
		logx.Warnf("MCP off: no model")
		return nil
	}
	servers, err := load(path)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.Go(func(life context.Context) { <-life.Done(); cancel() })
	return start(ctx, h, servers)
}

func load(path string) (map[string]server, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f struct {
		Servers map[string]server `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	for name, s := range f.Servers {
		if s.Disabled {
			delete(f.Servers, name) // not started, and not checked, so it can be parked half-written
			continue
		}
		if (s.URL == "") == (s.Command == "") {
			return nil, fmt.Errorf("MCP server %q needs a url or a command, not both or neither", name)
		}
	}
	return f.Servers, nil
}

// maxInstructions caps a server's own instructions in the system prompt.
const maxInstructions = 1000

// initialize performs the MCP handshake, as the bot's version, and returns
// the instructions the server sent, if any.
func initialize(ctx context.Context, c conn, version string) (string, error) {
	res, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": protocol,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]string{"name": "signalbot", "version": version},
	})
	if err != nil {
		return "", err
	}
	var r struct {
		Instructions string `json:"instructions"`
	}
	json.Unmarshal(res, &r) // instructions are optional; a bad result shows up in tools/list
	return strings.TrimSpace(r.Instructions), c.notify(ctx, "notifications/initialized", nil)
}

// tool is a tool as a server lists it.
type tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"inputSchema"`
}

// modelName is the name the model sees for a server's tool.
func modelName(server, tool string) string {
	n := unsafeToolChars.ReplaceAllString(server, "_") + "__" + unsafeToolChars.ReplaceAllString(tool, "_")
	return n[:min(len(n), 64)]
}

// start connects to every server, in name order, and offers each one's
// tools that its allowed_tools and blocked_tools let through as a tool
// group, named by those tools and then its chat commands. A server that cannot be reached is logged
// and left out. A tool named in either list that a reachable server does
// not have, or two tools that end up with the same name, is an error: a
// typo in blocked_tools would otherwise leave the tool offered. Local
// servers are stopped when ctx ends, and the bot waits for them. While the
// model can't call tools, no command is registered.
func start(ctx context.Context, h feature.Host, servers map[string]server) error {
	info := h.Info()
	version := cmp.Or(info.Version, "dev")
	taken := map[string]bool{} // the model's names for tools
	var tools, cmds []string   // for the startup lines
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		cfg := servers[name]
		var c conn = &httpConn{url: cfg.URL, headers: cfg.Headers, client: httpx.Client(2), version: version}
		if cfg.Command != "" {
			c = &stdioConn{name: name, cfg: cfg, version: version, redact: h.Redact}
		}
		listed, sent, err := connect(ctx, c, version)
		if err != nil {
			logx.Errorf("MCP server %s unavailable, its tools are left out: %v", name, err)
			continue
		}
		for _, want := range append(slices.Clone(cfg.AllowedTools), cfg.BlockedTools...) {
			if !slices.ContainsFunc(listed, func(t tool) bool { return t.Name == want }) {
				return fmt.Errorf("MCP server %q has no tool %q", name, want)
			}
		}
		g := feature.ToolGroup{
			System:   instructions(cfg.Instructions, sent),
			Keywords: keywords(cfg.Keywords),
		}
		models := map[string]string{} // the model's name for each offered tool, by the server's
		for _, t := range listed {
			if cfg.AllowedTools != nil && !slices.Contains(cfg.AllowedTools, t.Name) || slices.Contains(cfg.BlockedTools, t.Name) {
				continue
			}
			model := modelName(name, t.Name)
			if taken[model] {
				return fmt.Errorf("MCP tool %q of server %q clashes with another tool's name", t.Name, name)
			}
			taken[model], models[t.Name] = true, model
			g.Names = append(g.Names, t.Name)
			g.Tools = append(g.Tools, feature.Tool{Name: model, Description: t.Description, Schema: t.Schema,
				Run: func(ctx context.Context, args string) (string, []feature.Picture, error) {
					return call(ctx, c, t.Name, args)
				}})
		}
		tools = append(tools, g.Names...) // before its commands join them
		if !info.CallsTools {
			cfg.ChatCommands = nil // a command would force a tool the model can't call
		}
		var own []string // its chat commands, as "/name"
		for _, n := range slices.Sorted(maps.Keys(cfg.ChatCommands)) {
			if cmd := command(h, name, n, cfg.ChatCommands[n], models); cmd != "" {
				own = append(own, cmd)
			}
		}
		slices.Sort(own)
		cmds = append(cmds, own...)
		if len(g.Tools) > 0 {
			g.Names = append(g.Names, own...)
			g.Listing = name + ": " + strings.Join(g.Names, ", ")
			h.Tools(g)
		}
		if s, ok := c.(*stdioConn); ok {
			h.Go(func(context.Context) { s.supervise(ctx) })
		}
	}
	if len(tools) > 0 {
		logx.Noticef("MCP tools: %s", strings.Join(tools, ", "))
	}
	if len(cmds) > 0 {
		logx.Noticef("chat commands: %s", strings.Join(cmds, ", "))
	}
	return nil
}

// instructions is a server's system-prompt line: its config's
// "instructions" if set (even to ""), otherwise the server's own, capped.
func instructions(cfg *string, own string) string {
	if cfg != nil {
		return strings.TrimSpace(*cfg)
	}
	return txt.Clamp(own, maxInstructions)
}

// keywords is a pattern matching any of words as a whole word, ignoring
// capitals; nil without words.
func keywords(words []string) *regexp.Regexp {
	var alts []string
	for _, w := range words {
		if w = strings.TrimSpace(w); w != "" {
			alts = append(alts, regexp.QuoteMeta(w))
		}
	}
	if len(alts) == 0 {
		return nil
	}
	return regexp.MustCompile(`(?i)\b(?:` + strings.Join(alts, "|") + `)\b`)
}

// command registers chat command n of a server on h and returns it as
// "/name". One that can't be used (a tool the model isn't offered, or a bad
// or taken name) is logged and skipped, returning "".
func command(h feature.Host, server, n string, c chatCommand, models map[string]string) string {
	name := strings.ToLower(strings.TrimPrefix(n, "/"))
	model, offered := models[c.Tool]
	if !offered {
		logx.Warnf("MCP server %s: chat command /%s names tool %q, which isn't offered; skipped", server, txt.Clamp(name, 40), txt.Clamp(c.Tool, 64))
		return ""
	}
	err := feature.Command(h, name, func(_ context.Context, m feature.Message, args string) feature.Action {
		if c.RequireArg && args == "" {
			return feature.Action{Answer: "/" + name + " needs something after it."}
		}
		return feature.Action{ToolChoice: model}
	})
	if err != nil {
		logx.Warnf("MCP server %s: chat command %q is not a free name of letters, digits, _ or -; skipped", server, txt.Clamp(n, 40))
		return ""
	}
	return "/" + name
}

// connect starts the server, initializes it, and lists its tools by name.
func connect(ctx context.Context, c conn, version string) ([]tool, string, error) {
	ctx2, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if s, ok := c.(*stdioConn); ok {
		if err := s.start(ctx); err != nil {
			return nil, "", err
		}
	}
	own, err := initialize(ctx2, c, version)
	if err != nil {
		return nil, "", err
	}
	var tools []tool
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		res, err := c.call(ctx2, "tools/list", params)
		if err != nil {
			return nil, "", err
		}
		var page struct {
			Tools      []tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(res, &page); err != nil {
			return nil, "", fmt.Errorf("tools/list: %v", err)
		}
		tools = append(tools, page.Tools...)
		if cursor = page.NextCursor; cursor == "" {
			slices.SortFunc(tools, func(a, b tool) int { return strings.Compare(a.Name, b.Name) })
			return slices.CompactFunc(tools, func(a, b tool) bool { return a.Name == b.Name }), own, nil
		}
	}
}

// call runs a server's tool with the JSON arguments the model wrote, and
// returns the text to give back to the model and any pictures to send to
// the chat. err is for the log only, and never holds the arguments or the
// result.
func call(ctx context.Context, c conn, name, arguments string) (string, []feature.Picture, error) {
	args := map[string]any{}
	if strings.TrimSpace(arguments) != "" {
		if json.Unmarshal([]byte(arguments), &args) != nil {
			return "error: the arguments were not a JSON object", nil, errors.New("invalid arguments")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	res, err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		var re *rpcError
		if errors.As(err, &re) {
			return "error: " + txt.Clamp(re.Message, 500), nil, fmt.Errorf("rpc error %d", re.Code)
		}
		return "error: the tool could not be reached", nil, err
	}
	var r struct {
		Content []struct {
			Type     string `json:"type"`
			Text     string `json:"text"`
			Data     string `json:"data"` // base64, for an image
			MimeType string `json:"mimeType"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if json.Unmarshal(res, &r) != nil {
		return "error: the tool sent a malformed result", nil, errors.New("malformed result")
	}
	var parts []string
	var pics []feature.Picture
	for _, p := range r.Content {
		switch data, err := base64.StdEncoding.DecodeString(p.Data); {
		case p.Type == "text":
			parts = append(parts, p.Text)
		case p.Type == "image" && err == nil && img.Sendable(data) && len(pics) < feature.MaxPictures:
			pics = append(pics, feature.Picture{Data: data})
			parts = append(parts, "[image sent to the chat]")
		default:
			parts = append(parts, "[non-text content omitted]")
		}
	}
	out := txt.Clamp(strings.Join(parts, "\n"), maxResult)
	if r.IsError {
		return "error: " + out, nil, errors.New("the tool reported an error")
	}
	return out, pics, nil
}

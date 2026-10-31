package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/robdevops/signalbot/internal/kit/img"
	mcpc "github.com/robdevops/signalbot/internal/kit/mcp"
	"maps"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/robdevops/signalbot/internal/kit/logx"

	"github.com/robdevops/signalbot/internal/kit/txt"
)

const (
	// maxToolResultRunes caps what one tool call adds to the conversation.
	maxToolResultRunes = 16 << 10
)

// mcpServerConfig is one entry of MCP_CONFIG's "mcpServers": a URL for a
// remote server, or a command for a local one. allowed_tools, when present,
// offers the model only those tools; blocked_tools, when present, offers all
// but those. Without either, every tool the server lists is offered.
type mcpServerConfig struct {
	mcpc.Server
	AllowedTools []string               `json:"allowed_tools"` // nil: not in whitelist mode
	BlockedTools []string               `json:"blocked_tools"`
	Disabled     bool                   `json:"disabled"` // left out entirely
	ChatCommands map[string]chatCommand `json:"chat_commands"`
}

// chatCommand makes "/name ..." a turn in which the model must call tool.
// With require_arg, a bare "/name" is answered by the bot, not the model.
type chatCommand struct {
	Tool       string `json:"tool"`
	RequireArg bool   `json:"require_arg"`
}

// mcpCommand is a chat command ready to use: Tool is the model's name for it.
type mcpCommand struct {
	Tool       string
	RequireArg bool
	Server     string
}

var commandName = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

func loadMCPConfig(path string) (map[string]mcpServerConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f struct {
		Servers map[string]mcpServerConfig `json:"mcpServers"`
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

// mcpTool is a tool the model may call.
type mcpTool struct {
	Model       string // the name the model sees: server__tool
	Server      string
	Name        string // the server's name for it
	Description string
	Schema      json.RawMessage
}

// MCP holds the tools of every configured server that the model may call.
type MCP struct {
	list    []mcpTool // sorted by Model
	byModel map[string]mcpTool
	clients map[string]*mcpc.Client
	cmds    map[string]mcpCommand // chat command name, without the slash
	wg      sync.WaitGroup        // stdio supervisors
}

var unsafeToolChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// toolModelName is the name the model sees for a server's tool.
func toolModelName(server, tool string) string {
	n := unsafeToolChars.ReplaceAllString(server, "_") + "__" + unsafeToolChars.ReplaceAllString(tool, "_")
	return n[:min(len(n), 64)]
}

// StartMCP connects to every server and collects the tools its
// allowed_tools and blocked_tools let through. A server that cannot be
// reached is logged and left out. A tool named in either list that a
// reachable server does not have, or two tools that end up with the same
// name, is an error: a typo in blocked_tools would otherwise leave the tool
// offered. Stdio servers are stopped when ctx ends.
func StartMCP(ctx context.Context, servers map[string]mcpServerConfig, version string) (*MCP, error) {
	m := &MCP{byModel: map[string]mcpTool{}, clients: map[string]*mcpc.Client{}, cmds: map[string]mcpCommand{}}
	names := make([]string, 0, len(servers))
	for n := range servers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		cfg := servers[name]
		c, err := mcpc.Dial(ctx, name, cfg.Server, version)
		var list []mcpc.Tool
		if err == nil {
			list, err = c.Tools(ctx)
		}
		listed := map[string]mcpTool{}
		for _, t := range list {
			listed[t.Name] = mcpTool{Name: t.Name, Description: t.Description, Schema: t.Schema}
		}
		if err != nil {
			logx.Errorf("MCP server %s unavailable, its tools are left out: %v", name, err)
			continue
		}
		for _, want := range append(slices.Clone(cfg.AllowedTools), cfg.BlockedTools...) {
			if _, ok := listed[want]; !ok {
				return nil, fmt.Errorf("MCP server %q has no tool %q", name, want)
			}
		}
		for _, tn := range slices.Sorted(maps.Keys(listed)) {
			if cfg.AllowedTools != nil && !slices.Contains(cfg.AllowedTools, tn) || slices.Contains(cfg.BlockedTools, tn) {
				continue
			}
			t := listed[tn]
			t.Server, t.Model = name, toolModelName(name, tn)
			if _, dup := m.byModel[t.Model]; dup {
				return nil, fmt.Errorf("MCP tool %q of server %q clashes with another tool's name", tn, name)
			}
			m.byModel[t.Model] = t
			m.list = append(m.list, t)
		}
		m.addCommands(name, cfg.ChatCommands)
		m.clients[name] = c
		if c.Local() {
			m.wg.Add(1)
			go func() { defer m.wg.Done(); c.Supervise(ctx) }()
		}
	}
	sort.Slice(m.list, func(i, j int) bool { return m.list[i].Model < m.list[j].Model })
	return m, nil
}

// addCommands takes a server's chat commands. One that can't be used (a bad
// or taken name, or a tool the model isn't offered) is logged and skipped.
func (m *MCP) addCommands(server string, cmds map[string]chatCommand) {
	for _, n := range slices.Sorted(maps.Keys(cmds)) {
		c, name := cmds[n], strings.ToLower(strings.TrimPrefix(n, "/"))
		t, offered := m.byModel[toolModelName(server, c.Tool)]
		_, taken := m.cmds[name]
		switch {
		case !commandName.MatchString(name) || taken:
			logx.Warnf("MCP server %s: chat command %q is not a free name of letters, digits, _ or -; skipped", server, txt.Clamp(n, 40))
		case !offered || t.Server != server:
			logx.Warnf("MCP server %s: chat command /%s names tool %q, which isn't offered; skipped", server, name, txt.Clamp(c.Tool, 64))
		default:
			m.cmds[name] = mcpCommand{Tool: t.Model, RequireArg: c.RequireArg, Server: server}
		}
	}
}

// Commands returns the usable chat commands by name, without the slash.
func (m *MCP) Commands() map[string]mcpCommand {
	if m == nil {
		return nil
	}
	return m.cmds
}

// Names lists the tools the model may call by their servers' names, for {tools}.
func (m *MCP) Names() []string {
	if m == nil {
		return nil
	}
	var out []string
	for _, t := range m.list {
		out = append(out, t.Name)
	}
	return out
}

// ByServer lists the tools the model may call, grouped by server, as
// "server: tool, tool; server: tool", for the system prompt.
func (m *MCP) ByServer() string {
	if m == nil {
		return ""
	}
	var groups []string
	for i, t := range m.list { // sorted by server__tool, so grouped by server
		if i == 0 || m.list[i-1].Server != t.Server {
			groups = append(groups, t.Server+": "+t.Name)
		} else {
			groups[len(groups)-1] += ", " + t.Name
		}
	}
	return strings.Join(groups, "; ")
}

// Wait waits for stdio servers to stop after the context given to StartMCP ends.
func (m *MCP) Wait() {
	if m != nil {
		m.wg.Wait()
	}
}

// Call runs the tool the model named, with the JSON arguments it wrote, and
// returns the text to give back to the model and any images to send to the
// chat. err is for the log only, and never holds the arguments or the result.
func (m *MCP) Call(ctx context.Context, model, arguments string) (result string, images [][]byte, t mcpTool, err error) {
	t, ok := m.byModel[model]
	if !ok {
		return "error: there is no tool named " + model, nil, t, errors.New("unknown tool")
	}
	args := map[string]any{}
	if strings.TrimSpace(arguments) != "" {
		if json.Unmarshal([]byte(arguments), &args) != nil {
			return "error: the arguments were not a JSON object", nil, t, errors.New("invalid arguments")
		}
	}
	r, err := m.clients[t.Server].Call(ctx, t.Name, args)
	if errors.Is(err, mcpc.ErrMalformed) {
		return "error: the tool sent a malformed result", nil, t, errors.New("malformed result")
	}
	if err != nil {
		var re *mcpc.RPCError
		if errors.As(err, &re) {
			return "error: " + txt.Clamp(re.Message, 500), nil, t, fmt.Errorf("rpc error %d", re.Code)
		}
		return "error: the tool could not be reached", nil, t, err
	}
	var parts []string
	for _, c := range r.Content {
		switch pic, err := base64.StdEncoding.DecodeString(c.Data); {
		case c.Type == "text":
			parts = append(parts, c.Text)
		case c.Type == "image" && err == nil && img.Sendable(pic) && len(images) < img.MaxPerMessage:
			images = append(images, pic)
			parts = append(parts, "[image sent to the chat]")
		default:
			parts = append(parts, "[non-text content omitted]")
		}
	}
	out := txt.Clamp(strings.Join(parts, "\n"), maxToolResultRunes)
	if r.IsError {
		return "error: " + out, nil, t, errors.New("the tool reported an error")
	}
	return out, images, t, nil
}

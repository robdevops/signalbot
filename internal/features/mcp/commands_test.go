package mcp

import (
	"context"
	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/feature/featuretest"
	"github.com/robdevops/signalbot/internal/features/mcp/mcptest"
	"net/http/httptest"
	"testing"
)

// chat_commands in a server's config become commands for the tools it
// offers; a bad or taken name, or a tool that isn't offered, is skipped.
func TestChatCommandsConfig(t *testing.T) {
	srv := &mcptest.Server{}
	m, s := startFake(t, srv, "echo")
	m2, err := StartMCP(context.Background(), map[string]mcpServerConfig{"fake": {Server: Server{URL: s.URL}, AllowedTools: []string{"echo", "pic"},
		ChatCommands: map[string]chatCommand{
			"Echo":     {Tool: "echo", RequireArg: true},
			"/pic":     {Tool: "pic"},
			"example":  {Tool: "echo"},
			"bad name": {Tool: "echo"},
			"fail":     {Tool: "fail"}, // not offered
		}}}, "test")
	if err != nil {
		t.Fatal(err)
	}
	got := m2.Commands()
	// "example" passes here: whether a name is free is checked when it is
	// registered with the bot, where another feature may have it.
	if len(got) != 3 || got["echo"] != (mcpCommand{"fake__echo", true, "fake"}) || got["pic"] != (mcpCommand{"fake__pic", false, "fake"}) {
		t.Fatalf("commands %v", got)
	}
	if len(m.Commands()) != 0 || (*MCP)(nil).Commands() != nil {
		t.Fatal("commands without chat_commands")
	}
}

// The feature registers its tools and chat commands with the host: a bare
// command with require_arg is answered by the bot, otherwise the model must
// call the tool; a name another feature has is skipped.
func TestFeatureRegistersCommands(t *testing.T) {
	srv := httptest.NewServer(&mcptest.Server{})
	t.Cleanup(srv.Close)
	path := writeConfig(t, `{"mcpServers": {"fake": {"url": "`+srv.URL+`", "allowed_tools": ["echo", "pic"],
		"chat_commands": {"quote": {"tool": "echo", "require_arg": true}, "pic": {"tool": "pic"}, "example": {"tool": "echo"}}}}}`)
	h := featuretest.New(map[string]string{"MCP_CONFIG": path})
	h.Command("example", nil) // another feature's
	f := &Feature{Version: "test", Model: "m"}
	if err := f.Register(h); err != nil {
		t.Fatal(err)
	}
	if len(h.Groups) != 2 || h.Groups[0].Prompt != "MCP tools by server, fake: echo, pic" || !h.Groups[0].Grouped ||
		h.Groups[1].Prompt != "chat commands /pic, /quote" || h.Groups[1].Intro != "/pic, /quote" {
		t.Fatalf("groups %+v", h.Groups)
	}
	turn := feature.Turn{Message: chat.Message{Sender: chat.Person{Name: "Alice"}}}
	if c, ok := h.Commands["quote"](context.Background(), turn, ""); !ok || c.Text != "/quote needs something after it." || c.Tool != "" {
		t.Fatalf("bare /quote: %+v", c)
	}
	if c, ok := h.Commands["quote"](context.Background(), turn, "apple"); !ok || c.Tool != "fake__echo" {
		t.Fatalf("/quote apple: %+v", c)
	}
	res, err := h.Groups[0].Tools[0].Run(context.Background(), `{"q":"x"}`)
	if err != nil || res.Text != `echo {"q":"x"}` {
		t.Fatalf("tool run: %+v, %v", res, err)
	}
	// Without tool calls, no commands.
	h2 := featuretest.New(map[string]string{"MCP_CONFIG": path})
	h2.Capabilities.Tools = false
	(&Feature{Version: "test"}).Register(h2)
	if len(h2.Commands) != 0 {
		t.Fatalf("commands for a model without tool calls: %v", h2.Commands)
	}
}

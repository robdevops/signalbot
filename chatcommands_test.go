package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// chat_commands in a server's config become commands for the tools it
// offers; a bad or taken name, or a tool that isn't offered, is skipped.
func TestChatCommandsConfig(t *testing.T) {
	srv := &fakeMCP{}
	m, s := startFake(t, srv, "echo")
	m2, err := StartMCP(context.Background(), map[string]mcpServerConfig{"fake": {URL: s.URL, AllowedTools: []string{"echo", "pic"},
		ChatCommands: map[string]chatCommand{
			"Echo":     {Tool: "echo", RequireArg: true},
			"/pic":     {Tool: "pic"},
			"example":  {Tool: "echo"},
			"bad name": {Tool: "echo"},
			"fail":     {Tool: "fail"}, // not offered
		}}})
	if err != nil {
		t.Fatal(err)
	}
	got := m2.Commands()
	if len(got) != 2 || got["echo"] != (mcpCommand{"fake__echo", true}) || got["pic"] != (mcpCommand{"fake__pic", false}) {
		t.Fatalf("commands %v", got)
	}
	if len(m.Commands()) != 0 || (*MCP)(nil).Commands() != nil {
		t.Fatal("commands without chat_commands")
	}
}

func TestParseCommand(t *testing.T) {
	for text, want := range map[string][3]string{
		"/quote apple vs nvidia": {"quote", "apple vs nvidia", "true"},
		" /Quote ":               {"quote", "", "true"},
		"/":                      {"", "", "false"},
		"quote apple":            {"", "", "false"},
	} {
		n, r, ok := parseCommand(text)
		if n != want[0] || r != want[1] || (map[bool]string{true: "true", false: "false"})[ok] != want[2] {
			t.Errorf("%q: got %q %q %v", text, n, r, ok)
		}
	}
}

// A chat command's turn offers only its tool, required until the result is in.
func TestChatCommandForcesTool(t *testing.T) {
	f := &fakeModel{calls: 1}
	o := modelWithTools(t, f)
	if _, err := o.Complete(withTool(context.Background(), "fake__echo"), "s", []ChatMsg{{Role: "user", Content: "/echo hi"}}); err != nil {
		t.Fatal(err)
	}
	if len(f.bodies) != 2 {
		t.Fatalf("%d requests", len(f.bodies))
	}
	for i, b := range f.bodies {
		tools := b["tools"].([]any)
		if len(tools) != 1 || tools[0].(map[string]any)["function"].(map[string]any)["name"] != "fake__echo" {
			t.Errorf("request %d offered %v", i, tools)
		}
	}
	if c, _ := f.bodies[0]["tool_choice"].(map[string]any); c["function"].(map[string]any)["name"] != "fake__echo" {
		t.Errorf("first request's tool_choice %v", f.bodies[0]["tool_choice"])
	}
	if c, ok := f.bodies[1]["tool_choice"]; ok {
		t.Errorf("tool_choice after the result: %v", c)
	}
}

// With require_arg, a bare command is answered by the bot, not the model.
func TestChatCommandRequiresArg(t *testing.T) {
	b, ai, _, sent := photoBot(t, false)
	b.commands = map[string]mcpCommand{"quote": {Tool: "fake__echo", RequireArg: true}}
	deliver(b, parse(t, strings.Replace(dm, "Test DM", "/quote", 1)))
	select {
	case got := <-sent:
		if got != "/quote needs something after it." {
			t.Fatalf("sent %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no reply")
	}
	ai.mu.Lock()
	called := ai.msgs != nil
	ai.mu.Unlock()
	if called {
		t.Fatal("the model was asked")
	}
	deliver(b, parse(t, strings.Replace(dm, "Test DM", "/quote apple", 1)))
	waitSent(t, sent)
	ai.mu.Lock()
	defer ai.mu.Unlock()
	if ai.msgs == nil || ai.msgs[len(ai.msgs)-1].Content != "/quote apple" {
		t.Fatalf("model got %v", ai.msgs)
	}
}

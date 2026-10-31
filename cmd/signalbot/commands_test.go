package main

import (
	"strings"
	"testing"
)

// End to end: a bare chat command with require_arg is answered by the bot
// without the model; with text, the model gets the message.
func TestChatCommandRequiresArg(t *testing.T) {
	path := mcpServers(t, map[string][]string{"fake": {"echo"}}, map[string]string{"fake": `{"quote": {"tool": "echo", "require_arg": true}}`})
	b, ai, _, sent := photoBot(t, false, func(c *Config) { c.MCPConfig = path })
	deliver(b, parse(t, strings.Replace(dm, "Test DM", "/quote", 1)))
	if got := <-sent; got != "/quote needs something after it." {
		t.Fatalf("sent %q", got)
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

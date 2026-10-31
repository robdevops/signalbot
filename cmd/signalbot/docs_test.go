package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/feature"
)

// Every setting the core, the Signal network or a feature reads is in the
// README's configuration table.
func TestEverySettingIsDocumented(t *testing.T) {
	readme, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SIGNALCLI_SOCKET", "/x")
	t.Setenv("OPENROUTER_API_KEY", "k")
	t.Setenv("MCP_CONFIG", "")
	env := &config.Env{}
	cfg, err := config.Load(env)
	if err != nil {
		t.Fatal(err)
	}
	env.Bool("MODEL_SEARCH", true) // main reads it for the startup line
	b := bot.New(context.Background(), &bot.Config{Model: cfg.Model, MaxConcurrency: 1, HistoryTurns: 1, Env: env,
		Caps: feature.Caps{Vision: true, Tools: true, ServerTools: true}}, nil)
	fs, _ := features(cfg)
	if err := b.Register(fs...); err != nil {
		t.Fatal(err)
	}
	for _, name := range env.Names() {
		if !strings.Contains(string(readme), "| `"+name+"` |") {
			t.Errorf("%s is read but not in the README's configuration table", name)
		}
	}
	if len(env.Names()) < 15 {
		t.Fatalf("only %d settings read: %v", len(env.Names()), env.Names())
	}
}

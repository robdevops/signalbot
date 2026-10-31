package main

import (
	"context"
	"flag"
	"github.com/robdevops/signalbot/internal/bot"
	"os"
	"path/filepath"
	"testing"

	"github.com/robdevops/signalbot/internal/transports/signal"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// The whole system prompt, pinned before the recode splits its parts across
// the core, the Signal adapter and the features: it must stay byte-identical.
func TestGoldenSystemPrompt(t *testing.T) {
	mcpConfig := mcpServers(t, map[string][]string{"yahoo": {"get_quote"}, "scout": {"filings"}},
		map[string]string{"yahoo": `{"quote": {"tool": "get_quote", "require_arg": true}}`})
	build := func(noTools bool) *bot.Bot {
		cfg := &Config{Model: "deepseek/deepseek-v4.1-flash-1017", Search: true, MaxConcurrency: 1, HistoryTurns: 10,
			Intro: defaultIntro, ExtraPrompt: "Answer in British English.", MCPConfig: mcpConfig, NoTools: noTools}
		sig := signal.NewAdapter("x", cfg.AllowedSenders)
		sig.Client().SetSelf(self)
		sig.SetContacts([]signal.Contact{{UUID: self, GivenName: "Botty"}})
		return NewBot(context.Background(), cfg, nil, sig)
	}
	b := build(false)
	cases := map[string]string{
		"prompt_chart.txt": prompt(b, true),
		"prompt_plain.txt": prompt(b, false),
	}
	cases["prompt_notools.txt"] = prompt(build(true), false)
	for name, got := range cases {
		path := filepath.Join("testdata", name)
		if *update {
			os.WriteFile(path, []byte(got), 0o644)
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v (run go test -run Golden -update once)", name, err)
		}
		if got != string(want) {
			t.Errorf("%s changed:\ngot  %q\nwant %q", name, got, want)
		}
	}
}

package main

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/robdevops/signalbot/internal/transports/signal"
)

var update = flag.Bool("update", false, "rewrite the golden files")

// The whole system prompt, pinned before the recode splits its parts across
// the core, the Signal adapter and the features: it must stay byte-identical.
func TestGoldenSystemPrompt(t *testing.T) {
	cfg := &Config{Model: "deepseek/deepseek-v4.1-flash-1017", Search: true, MaxConcurrency: 1, HistoryTurns: 10,
		Intro: defaultIntro, ExtraPrompt: "Answer in British English."}
	sig := signal.NewAdapter("x", cfg.AllowedSenders)
	sig.Client().SetSelf(self)
	b := NewBot(context.Background(), cfg, nil, sig)
	adapterOf(b).SetContacts([]signal.Contact{{UUID: self, GivenName: "Botty"}})
	b.toolNames, b.toolGroups = []string{"get_quote", "filings"}, "yahoo: get_quote; scout: filings"
	b.commands = map[string]mcpCommand{"quote": {Tool: "yahoo__get_quote", RequireArg: true}}
	cases := map[string]string{
		"prompt_chart.txt": b.prompt(true),
		"prompt_plain.txt": b.prompt(false),
	}
	b.toolCalls = false
	cases["prompt_notools.txt"] = b.prompt(false)
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

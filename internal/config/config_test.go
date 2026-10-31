package config

import (
	"testing"

	"github.com/robdevops/signalbot/internal/kit/logx"
)

func TestConfigParsing(t *testing.T) {
	t.Setenv("SIGNALCLI_SOCKET", "/x")
	t.Setenv("OPENROUTER_API_KEY", "k")

	t.Setenv("SIGNAL_DM_USER_IDS", "uuid-a,uuid-b\r\n\tuuid-c;uuid-d\r")
	c, err := Load(&Env{})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"uuid-a", "uuid-b", "uuid-c", "uuid-d"} {
		if !c.AllowedSenders[id] {
			t.Errorf("%s missing from allow-list %#v", id, c.AllowedSenders)
		}
	}
	if len(c.AllowedSenders) != 4 {
		t.Errorf("stray tokens in allow-list: %#v", c.AllowedSenders)
	}

	for v, want := range map[string]bool{
		"true": true, "True": true, "TRUE": true, "1": true, "yes": true, "on": true,
		"false": false, "False": false, "0": false, "no": false, "off": false, "garbage": true,
	} {
		t.Setenv("MODEL_SEARCH", v)
		if got := (&Env{}).Bool("MODEL_SEARCH", true); got != want {
			t.Errorf("MODEL_SEARCH=%q: got %v, want %v", v, got, want)
		}
	}

	t.Setenv("SYSTEM_PROMPT", "")
	c, _ = Load(&Env{})
	if c.ExtraPrompt != "" {
		t.Error(`SYSTEM_PROMPT must default to ""`)
	}
}

// OPENROUTER_API_KEY is required, and MODEL has a default.
func TestConfigKeyAndModel(t *testing.T) {
	t.Setenv("SIGNALCLI_SOCKET", "/x")
	for _, v := range []string{"OPENROUTER_API_KEY", "MODEL", "MODEL_SEARCH", "REASONING"} {
		t.Setenv(v, "")
	}
	if _, err := Load(&Env{}); err == nil {
		t.Fatal("no API key must be a config error")
	}
	t.Setenv("OPENROUTER_API_KEY", "k")
	c, err := Load(&Env{})
	if err != nil {
		t.Fatal(err)
	}
	if c.APIKey != "k" || c.Model != "deepseek/deepseek-v4.1-flash-1017" || c.Reasoning != "low" {
		t.Fatalf("defaults: key %q model %q reasoning %q", c.APIKey, c.Model, c.Reasoning)
	}
	t.Setenv("MODEL", "x-ai/grok-4")
	if c, _ := Load(&Env{}); c.Model != "x-ai/grok-4" {
		t.Fatalf("MODEL not honoured: %q", c.Model)
	}
}

// MODEL_API_URL points at another OpenAI-compatible provider; its key is
// MODEL_API_KEY, which a local server may not need.
func TestConfigProvider(t *testing.T) {
	t.Setenv("SIGNALCLI_SOCKET", "/x")
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("MODEL_API_KEY", "")
	t.Setenv("MODEL_API_URL", "http://localhost:11434/v1")
	c, err := Load(&Env{})
	if err != nil || c.APIURL != "http://localhost:11434/v1" || c.APIKey != "" {
		t.Fatalf("a local provider without a key: %+v, %v", c, err)
	}
	t.Setenv("MODEL_API_URL", "")
	t.Setenv("OPENROUTER_API_KEY", "old")
	t.Setenv("MODEL_API_KEY", "new")
	if c, _ := Load(&Env{}); c.APIKey != "new" || c.APIURL != "https://openrouter.ai/api/v1" {
		t.Fatalf("MODEL_API_KEY wins, and OpenRouter is the default: %+v", c)
	}
}

func TestLogLevelConfig(t *testing.T) {
	t.Setenv("SIGNALCLI_SOCKET", "/x")
	t.Setenv("OPENROUTER_API_KEY", "k")
	for v, want := range map[string]logx.Level{"": logx.LevelInfo, "debug": logx.LevelDebug, "INFO": logx.LevelInfo, "warn": logx.LevelWarn, "warning": logx.LevelWarn, "error": logx.LevelError} {
		t.Setenv("LOG_LEVEL", v)
		c, err := Load(&Env{})
		if err != nil || c.LogLevel != want {
			t.Errorf("LOG_LEVEL=%q: Level %d, err %v; want %d", v, c.LogLevel, err, want)
		}
	}
	t.Setenv("LOG_LEVEL", "verbose")
	if _, err := Load(&Env{}); err == nil {
		t.Error("an unknown LOG_LEVEL must be a config error")
	}
}

// Every setting read is recorded, for the README check.
func TestEnvRecordsNames(t *testing.T) {
	e := &Env{}
	e.String("B", "")
	e.Bool("A", false)
	if got := e.Names(); len(got) != 2 || got[0] != "A" || got[1] != "B" {
		t.Fatalf("names %v", got)
	}
}

// No network configured is a config error that says how to configure one.
func TestAtLeastOneNetwork(t *testing.T) {
	t.Setenv("SIGNALCLI_SOCKET", "")
	t.Setenv("OPENROUTER_API_KEY", "k")
	if _, err := Load(&Env{}); err == nil || err.Error() != "configure at least one chat network: SIGNALCLI_SOCKET is required" {
		t.Fatalf("got %v", err)
	}
}

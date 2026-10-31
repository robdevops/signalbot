package main

import (
	"testing"
)

// OPENROUTER_API_KEY is required, and MODEL has a default.
func TestConfigKeyAndModel(t *testing.T) {
	t.Setenv("SIGNALCLI_SOCKET", "/x")
	for _, v := range []string{"OPENROUTER_API_KEY", "MODEL", "MODEL_SEARCH", "REASONING"} {
		t.Setenv(v, "")
	}
	if _, err := LoadConfig(); err == nil {
		t.Fatal("no API key must be a config error")
	}
	t.Setenv("OPENROUTER_API_KEY", "k")
	c, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if c.APIKey != "k" || c.Model != "deepseek/deepseek-v4.1-flash-1017" || !c.Search || c.Reasoning != "low" {
		t.Fatalf("defaults: key %q model %q search %v reasoning %q", c.APIKey, c.Model, c.Search, c.Reasoning)
	}
	t.Setenv("MODEL", "x-ai/grok-4")
	if c, _ := LoadConfig(); c.Model != "x-ai/grok-4" {
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
	c, err := LoadConfig()
	if err != nil || c.APIURL != "http://localhost:11434/v1" || c.APIKey != "" {
		t.Fatalf("a local provider without a key: %+v, %v", c, err)
	}
	t.Setenv("MODEL_API_URL", "")
	t.Setenv("OPENROUTER_API_KEY", "old")
	t.Setenv("MODEL_API_KEY", "new")
	if c, _ := LoadConfig(); c.APIKey != "new" || c.APIURL != "https://openrouter.ai/api/v1" {
		t.Fatalf("MODEL_API_KEY wins, and OpenRouter is the default: %+v", c)
	}
}

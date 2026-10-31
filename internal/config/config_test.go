package config

import (
	"slices"
	"testing"
)

func setRequired(t *testing.T) {
	t.Setenv("SIGNALCLI_SOCKET", "/run/signal.sock")
	t.Setenv("OPENROUTER_API_KEY", "key")
}

func TestDefaults(t *testing.T) {
	setRequired(t)
	for _, k := range []string{"MODEL", "MODEL_REASONING", "MODEL_SYSTEM_PROMPT", "INTRO", "MAX_CONCURRENCY", "HISTORY_TURNS", "SIGNAL_DM_USER_IDS"} {
		t.Setenv(k, "")
	}
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Model != "deepseek/deepseek-v4.1-flash-1017" || c.Reasoning != "low" || c.Intro != DefaultIntro ||
		c.MaxConcurrency != 32 || c.HistoryTurns != 10 || c.SystemPrompt != "" || len(c.AllowedSenders) != 0 {
		t.Errorf("got %+v", c)
	}
}

func TestSettings(t *testing.T) {
	setRequired(t)
	t.Setenv("MODEL_REASONING", " HIGH ")
	t.Setenv("MODEL_SYSTEM_PROMPT", "Be brief.")
	t.Setenv("MAX_CONCURRENCY", "-3")
	t.Setenv("HISTORY_TURNS", "4")
	t.Setenv("SIGNAL_DM_USER_IDS", "alice-id,bob-id;\r\n+15550001\t")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Reasoning != "high" || c.SystemPrompt != "Be brief." || c.MaxConcurrency != 32 || c.HistoryTurns != 4 {
		t.Errorf("got %+v", c)
	}
	if len(c.AllowedSenders) != 3 || !c.AllowedSenders["alice-id"] || !c.AllowedSenders["bob-id"] || !c.AllowedSenders["+15550001"] {
		t.Errorf("allowed %v", c.AllowedSenders)
	}
	for _, k := range []string{"MODEL_REASONING", "MODEL_SYSTEM_PROMPT", "SIGNAL_DM_USER_IDS"} {
		if !slices.Contains(Read(), k) {
			t.Errorf("%s not recorded", k)
		}
	}
}

func TestRequired(t *testing.T) {
	setRequired(t)
	t.Setenv("SIGNALCLI_SOCKET", "")
	if _, err := Load(); err == nil {
		t.Error("no socket accepted")
	}
	setRequired(t)
	t.Setenv("OPENROUTER_API_KEY", " ")
	if _, err := Load(); err == nil {
		t.Error("no API key accepted")
	}
}

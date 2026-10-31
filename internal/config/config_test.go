package config

import (
	"slices"
	"testing"
)

func setRequired(t *testing.T) {
	t.Setenv("SIGNALCLI_SOCKET", "/run/signal.sock")
	t.Setenv("MODEL_API_KEY", "key")
	t.Setenv("MODEL_API_URL", "")
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
	if c.APIURL != "https://openrouter.ai/api/v1" || c.APIKey != "key" || c.NoModel || c.Model != "deepseek/deepseek-v4.1-flash-1017" || c.Reasoning != "low" || c.Intro != DefaultIntro ||
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
}

// A key alone is OpenRouter; a URL alone is a provider that needs no key;
// with neither there is no model.
func TestModelSettings(t *testing.T) {
	for _, tc := range []struct {
		url, key, wantURL string
		noModel           bool
	}{
		{"", "key", "https://openrouter.ai/api/v1", false},
		{" http://localhost:11434/v1/ ", "", "http://localhost:11434/v1", false},
		{"https://api.example.com/v1", "key", "https://api.example.com/v1", false},
		{"", " ", "https://openrouter.ai/api/v1", true},
	} {
		setRequired(t)
		t.Setenv("MODEL_API_URL", tc.url)
		t.Setenv("MODEL_API_KEY", tc.key)
		c, err := Load()
		if err != nil {
			t.Fatal(err)
		}
		if c.APIURL != tc.wantURL || c.NoModel != tc.noModel {
			t.Errorf("%q, %q: got URL %q, no model %v", tc.url, tc.key, c.APIURL, c.NoModel)
		}
	}
	for _, k := range []string{"MODEL_API_URL", "MODEL_API_KEY"} {
		if !slices.Contains(Read(), k) {
			t.Errorf("%s not recorded", k)
		}
	}
}

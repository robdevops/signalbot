package config

import "testing"

func setRequired(t *testing.T) {
	t.Setenv("MODEL_API_KEY", "key")
	t.Setenv("MODEL_API_URL", "")
}

func TestDefaults(t *testing.T) {
	setRequired(t)
	for _, k := range []string{"MODEL", "MODEL_REASONING", "MODEL_SYSTEM_PROMPT", "INTRO", "MAX_CONCURRENCY", "HISTORY_TURNS"} {
		t.Setenv(k, "")
	}
	c := Load()
	if c.APIURL != "https://openrouter.ai/api/v1" || c.APIKey != "key" || c.NoModel || c.Model != "deepseek/deepseek-v4.1-flash-1017" || c.Reasoning != "low" || c.Intro != DefaultIntro ||
		c.MaxConcurrency != 32 || c.HistoryTurns != 10 || c.SystemPrompt != "" {
		t.Errorf("got %+v", c)
	}
}

func TestSettings(t *testing.T) {
	setRequired(t)
	t.Setenv("MODEL_REASONING", " HIGH ")
	t.Setenv("MODEL_SYSTEM_PROMPT", "Be brief.")
	t.Setenv("MAX_CONCURRENCY", "-3")
	t.Setenv("HISTORY_TURNS", "4")
	c := Load()
	if c.Reasoning != "high" || c.SystemPrompt != "Be brief." || c.MaxConcurrency != 32 || c.HistoryTurns != 4 {
		t.Errorf("got %+v", c)
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
		c := Load()
		if c.APIURL != tc.wantURL || c.NoModel != tc.noModel {
			t.Errorf("%q, %q: got URL %q, no model %v", tc.url, tc.key, c.APIURL, c.NoModel)
		}
	}
}

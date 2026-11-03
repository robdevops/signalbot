package config

import "testing"

func setRequired(t *testing.T) {
	t.Setenv("MODEL_API_KEY", "key")
	t.Setenv("MODEL_API_URL", "")
}

func TestDefaults(t *testing.T) {
	setRequired(t)
	for _, k := range []string{"MODEL", "MODEL_REASONING", "MODEL_SYSTEM_PROMPT", "INTRO", "MAX_CONCURRENCY", "HISTORY_MAX", "TOOL_LINGER_ROUNDS"} {
		t.Setenv(k, "")
	}
	c := Load()
	if c.APIURL != "https://openrouter.ai/api/v1" || c.APIKey != "key" || c.NoModel || c.Model != "deepseek/deepseek-v4.1-flash-1017" || c.Reasoning != "low" || c.Intro != DefaultIntro ||
		c.MaxConcurrency != 32 || c.HistoryMax != 10 || c.ToolLinger != 2 || c.SystemPrompt != "" {
		t.Errorf("got %+v", c)
	}
}

func TestSettings(t *testing.T) {
	setRequired(t)
	t.Setenv("MODEL_REASONING", " HIGH ")
	t.Setenv("MODEL_SYSTEM_PROMPT", "Be brief.")
	t.Setenv("MAX_CONCURRENCY", "-3")
	t.Setenv("HISTORY_MAX", "4")
	c := Load()
	if c.Reasoning != "high" || c.SystemPrompt != "Be brief." || c.MaxConcurrency != 32 || c.HistoryMax != 4 {
		t.Errorf("got %+v", c)
	}
}

// TOOL_LINGER_ROUNDS may be 0, is at most HISTORY_MAX, and is 2 if unset or
// not a number of 0 or more.
func TestToolLinger(t *testing.T) {
	setRequired(t)
	for _, tc := range []struct {
		linger, max string
		want        int
	}{{"", "", 2}, {"0", "", 0}, {" 5 ", "", 5}, {"50", "", 10}, {"8", "4", 4}, {"-1", "", 2}, {"lots", "", 2}, {"", "1", 1}} {
		t.Setenv("TOOL_LINGER_ROUNDS", tc.linger)
		t.Setenv("HISTORY_MAX", tc.max)
		if got := Load().ToolLinger; got != tc.want {
			t.Errorf("TOOL_LINGER_ROUNDS=%q HISTORY_MAX=%q: %d, want %d", tc.linger, tc.max, got, tc.want)
		}
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

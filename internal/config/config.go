// Package config reads the core's settings and the chat networks', from
// the environment. Features read their own through feature.Env.
package config

import (
	"fmt"
	"strings"

	"github.com/robdevops/signalbot/internal/kit/logx"
)

// OpenRouterURL is the default model API: OpenRouter's.
const OpenRouterURL = "https://openrouter.ai/api/v1"

// Config is the core's settings, and the Signal network's.
type Config struct {
	APIURL         string // MODEL_API_URL: the OpenAI-compatible API, OpenRouter by default
	APIKey         string // MODEL_API_KEY, else OPENROUTER_API_KEY
	Model          string
	Reasoning      string
	ExtraPrompt    string // SYSTEM_PROMPT: appended to the built-in prompt
	MaxConcurrency int
	HistoryTurns   int
	LogLevel       logx.Level

	Socket         string          // SIGNALCLI_SOCKET: turns the Signal network on
	AllowedSenders map[string]bool // SIGNAL_DM_USER_IDS
}

// Load reads the settings through e.
func Load(e *Env) (*Config, error) {
	c := &Config{
		APIURL:         e.String("MODEL_API_URL", OpenRouterURL),
		APIKey:         e.String("MODEL_API_KEY", e.String("OPENROUTER_API_KEY", "")),
		Model:          e.String("MODEL", "deepseek/deepseek-v4.1-flash-1017"),
		Reasoning:      strings.ToLower(e.String("REASONING", "low")),
		ExtraPrompt:    e.String("SYSTEM_PROMPT", ""),
		MaxConcurrency: e.Int("MAX_CONCURRENCY", 32),
		HistoryTurns:   e.Int("HISTORY_TURNS", 10),
		Socket:         e.String("SIGNALCLI_SOCKET", ""),
		AllowedSenders: e.IDs("SIGNAL_DM_USER_IDS"),
	}
	var ok bool
	if c.LogLevel, ok = logx.ParseLevel(e.String("LOG_LEVEL", "")); !ok {
		return nil, fmt.Errorf("LOG_LEVEL must be debug, info, warn or error")
	}
	if c.Socket == "" {
		return nil, fmt.Errorf("configure at least one chat network: SIGNALCLI_SOCKET is required")
	}
	if c.APIKey == "" && c.APIURL == OpenRouterURL {
		return nil, fmt.Errorf("OPENROUTER_API_KEY is required")
	}
	return c, nil
}

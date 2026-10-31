// Package config reads the core's settings and the chat networks', from
// the environment. Features read their own through feature.Env.
package config

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"

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
	Intro          string // INTRO
	Search         bool   // MODEL_SEARCH: OpenRouter's web search
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
		Intro:          e.String("INTRO", ""),
		Search:         e.Bool("MODEL_SEARCH", true),
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

// Env reads settings from the environment and records every name read, so
// a test can check the README documents each one.
type Env struct {
	mu   sync.Mutex
	read map[string]bool
}

func (e *Env) note(name string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.read == nil {
		e.read = map[string]bool{}
	}
	e.read[name] = true
}

// Names lists the settings read so far, sorted.
func (e *Env) Names() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []string
	for n := range e.read {
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// String is the setting, trimmed, or def if it is empty.
func (e *Env) String(name, def string) string {
	e.note(name)
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

// Int is a positive whole number, or def.
func (e *Env) Int(name string, def int) int {
	if n, err := strconv.Atoi(e.String(name, "")); err == nil && n > 0 {
		return n
	}
	return def
}

// Bool accepts the spellings strconv.ParseBool does, case-insensitively,
// plus yes/no/on/off. An unparseable value falls back to def.
func (e *Env) Bool(name string, def bool) bool {
	switch strings.ToLower(e.String(name, "")) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	case "0", "f", "false", "n", "no", "off":
		return false
	}
	return def
}

// IDs splits a list on commas, semicolons, spaces, tabs and line breaks, so
// a CRLF env file or a tab-separated list cannot leave an unmatchable id.
func (e *Env) IDs(name string) map[string]bool {
	out := map[string]bool{}
	for _, s := range strings.FieldsFunc(e.String(name, ""), func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	}) {
		out[s] = true
	}
	return out
}

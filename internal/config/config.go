// Package config reads the core's settings from the environment. Every
// setting read, the core's or a feature's, is recorded for the docs test.
package config

import (
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// DefaultIntro is the self-introduction when INTRO is unset. {name},
// {model} and {tools} are filled in.
const DefaultIntro = "hi, I'm {name}, your open source assistant running {model} (tools: {tools}). " +
	"I respond to @mentions and replies to my messages. See " +
	"https://github.com/robdevops/signalbot/blob/main/README.md for details."

// Config is the core's settings.
type Config struct {
	Socket         string          // SIGNALCLI_SOCKET: a unix socket path, or host:port
	APIKey         string          // OPENROUTER_API_KEY
	Model          string          // MODEL, as "provider/model"
	Reasoning      string          // MODEL_REASONING: the reasoning effort, "none" to leave it out
	SystemPrompt   string          // MODEL_SYSTEM_PROMPT: added to the built-in prompt
	Intro          string          // INTRO
	AllowedSenders map[string]bool // SIGNAL_DM_USER_IDS: who may DM the bot; changed in place by tests
	MaxConcurrency int             // MAX_CONCURRENCY: messages answered at once
	HistoryTurns   int             // HISTORY_TURNS: exchanges remembered per chat
}

var (
	mu   sync.Mutex
	read = map[string]bool{}
)

// Env is the setting key, trimmed, or def if it is unset or blank.
func Env(key, def string) string {
	mu.Lock()
	read[key] = true
	mu.Unlock()
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// Int is the setting key as a positive number, else def.
func Int(key string, def int) int {
	if n, err := strconv.Atoi(Env(key, "")); err == nil && n > 0 {
		return n
	}
	return def
}

// Read is every setting read so far, sorted.
func Read() []string {
	mu.Lock()
	defer mu.Unlock()
	var out []string
	for k := range read {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// Load reads the core's settings.
func Load() (*Config, error) {
	c := &Config{
		Socket:         Env("SIGNALCLI_SOCKET", ""),
		APIKey:         Env("OPENROUTER_API_KEY", ""),
		Model:          Env("MODEL", "deepseek/deepseek-v4.1-flash-1017"),
		Reasoning:      strings.ToLower(Env("MODEL_REASONING", "low")),
		SystemPrompt:   Env("MODEL_SYSTEM_PROMPT", ""),
		Intro:          Env("INTRO", DefaultIntro),
		AllowedSenders: map[string]bool{},
		MaxConcurrency: Int("MAX_CONCURRENCY", 32),
		HistoryTurns:   Int("HISTORY_TURNS", 10),
	}
	// Split on commas, semicolons and any white space, so a CRLF env file or
	// a tab-separated list cannot leave an unmatchable ID in the list.
	for _, id := range strings.FieldsFunc(Env("SIGNAL_DM_USER_IDS", ""), func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	}) {
		c.AllowedSenders[id] = true
	}
	if c.Socket == "" {
		return nil, errors.New("SIGNALCLI_SOCKET is required")
	}
	if c.APIKey == "" {
		return nil, errors.New("OPENROUTER_API_KEY is required")
	}
	return c, nil
}

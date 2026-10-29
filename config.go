package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Socket         string
	APIKey         string // OPENROUTER_API_KEY
	Model          string
	Reasoning      string
	Search         bool
	AllowedSenders map[string]bool
	ExtraPrompt    string // SYSTEM_PROMPT: appended to the built-in prompt
	Intro          string // INTRO: {name}, {model} and {tools} are substituted
	MaxConcurrency int
	HistoryTurns   int
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(env(key, "")); err == nil && n > 0 {
		return n
	}
	return def
}

// envBool accepts the spellings strconv.ParseBool does, case-insensitively,
// plus yes/no/on/off. An unparseable value falls back to def.
func envBool(key string, def bool) bool {
	switch strings.ToLower(env(key, "")) {
	case "1", "t", "true", "y", "yes", "on":
		return true
	case "0", "f", "false", "n", "no", "off":
		return false
	}
	return def
}

// splitIDs splits on commas, semicolons, spaces, tabs and line breaks, so a
// CRLF env file or a tab-separated list cannot leave an unmatchable id in the
// allow-list.
func splitIDs(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
}

func LoadConfig() (*Config, error) {
	c := &Config{
		Socket:         env("SIGNALCLI_SOCKET", ""),
		APIKey:         env("OPENROUTER_API_KEY", ""),
		Model:          env("MODEL", "deepseek/deepseek-v4.1-flash-1017"),
		Search:         envBool("MODEL_SEARCH", true),
		Reasoning:      strings.ToLower(env("REASONING", "low")),
		AllowedSenders: map[string]bool{},
		ExtraPrompt:    env("SYSTEM_PROMPT", ""),
		Intro:          env("INTRO", defaultIntro),
		MaxConcurrency: envInt("MAX_CONCURRENCY", 32),
		HistoryTurns:   envInt("HISTORY_TURNS", 10),
	}
	for _, s := range splitIDs(os.Getenv("SIGNAL_DM_USER_IDS")) {
		c.AllowedSenders[s] = true
	}
	if c.Socket == "" {
		return nil, fmt.Errorf("SIGNALCLI_SOCKET is required")
	}
	if c.APIKey == "" {
		return nil, fmt.Errorf("OPENROUTER_API_KEY is required")
	}
	return c, nil
}

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Socket         string
	APIKey         string
	Model          string
	Reasoning      string
	Search         bool
	AllowedSenders map[string]bool
	SystemPrompt   string
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

func LoadConfig() (*Config, error) {
	c := &Config{
		Socket:         env("SIGNALCLI_SOCKET", ""),
		APIKey:         env("XAI_API_KEY", ""),
		Model:          env("MODEL", "grok-4.3"),
		Reasoning:      strings.ToLower(env("REASONING", "low")),
		Search:         env("XAI_SEARCH", "true") == "true" || env("XAI_SEARCH", "") == "1",
		AllowedSenders: map[string]bool{},
		SystemPrompt: env("SYSTEM_PROMPT", "You are a helpful, concise assistant in a Signal chat. "+
			"Reply in plain text (no markdown), keep answers short unless asked for detail."),
		MaxConcurrency: envInt("MAX_CONCURRENCY", 32),
		HistoryTurns:   envInt("HISTORY_TURNS", 10),
	}
	for _, s := range strings.FieldsFunc(os.Getenv("SIGNAL_ALLOWED_SENDERS"), func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n'
	}) {
		c.AllowedSenders[s] = true
	}
	if c.Socket == "" {
		return nil, fmt.Errorf("SIGNALCLI_SOCKET is required")
	}
	if c.APIKey == "" {
		return nil, fmt.Errorf("XAI_API_KEY is required")
	}
	return c, nil
}

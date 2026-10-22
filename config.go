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
	ExtraPrompt    string // SYSTEM_PROMPT: appended to the built-in prompt
	Intro          string // INTRO: {name} and {model} are substituted
	BotName        string
	LogMessageBody bool
	PriceIn        float64 // USD per million input tokens
	PriceOut       float64 // USD per million output tokens
	MaxConcurrency int
	HistoryTurns   int
	MaxChats       int
	DedupeWindow   int // seconds
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

func envFloat(key string, def float64) float64 {
	if f, err := strconv.ParseFloat(env(key, ""), 64); err == nil && f >= 0 {
		return f
	}
	return def
}

// envBool accepts the spellings strconv.ParseBool does, case-insensitively,
// plus yes/no/on/off. An unparseable value falls back to def.
func envBool(key string, def bool) bool {
	v := strings.ToLower(env(key, ""))
	switch v {
	case "":
		return def
	case "yes", "y", "on":
		return true
	case "no", "n", "off":
		return false
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

// splitIDs splits on commas and any whitespace, so a CRLF env file or a
// tab-separated list cannot leave an unmatchable id in the allow-list.
func splitIDs(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\t' || r == '\n' || r == '\r'
	})
}

func LoadConfig() (*Config, error) {
	c := &Config{
		Socket:         env("SIGNALCLI_SOCKET", ""),
		APIKey:         env("XAI_API_KEY", ""),
		Model:          env("MODEL", "grok-4.3"),
		Reasoning:      strings.ToLower(env("REASONING", "low")),
		Search:         envBool("XAI_SEARCH", true),
		AllowedSenders: map[string]bool{},
		ExtraPrompt:    env("SYSTEM_PROMPT", ""),
		Intro:          env("INTRO", defaultIntro),
		BotName:        env("BOT_NAME", ""),
		LogMessageBody: envBool("LOG_MESSAGE_BODY", false),
		PriceIn:        envFloat("XAI_PRICE_IN", 1.25),
		PriceOut:       envFloat("XAI_PRICE_OUT", 2.50),
		MaxConcurrency: envInt("MAX_CONCURRENCY", 32),
		HistoryTurns:   envInt("HISTORY_TURNS", 10),
		MaxChats:       envInt("MAX_CHATS", 500),
		DedupeWindow:   envInt("DEDUPE_SECONDS", 10),
	}
	for _, s := range splitIDs(os.Getenv("SIGNAL_ALLOWED_SENDERS")) {
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

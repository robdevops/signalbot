// Package config reads settings from the environment, the core's in Load,
// and holds the tests' hooks.
package config

import (
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// DefaultIntro is the self-introduction when INTRO is unset. {name},
// {model}, {tools} and {more} (features' lines) are filled in.
const DefaultIntro = "hi, I'm *{name}*, your open source assistant running {model}.\n\n" +
	"Tools:\n{tools}\n\n{more}" +
	"I respond to @{name} and replies to my messages.\n\n" +
	"See https://github.com/robdevops/signalbot/blob/main/README.md for details."

// Config is the core's settings.
type Config struct {
	APIURL         string // MODEL_API_URL: the chat completions API's base URL
	APIKey         string // MODEL_API_KEY; "" sends none
	NoModel        bool   // neither MODEL_API_URL nor MODEL_API_KEY is set: only commands are answered
	Model          string // MODEL: the model's ID at the API, such as "provider/model" on OpenRouter
	Reasoning      string // MODEL_REASONING: the reasoning effort, "none" to leave it out
	SystemPrompt   string // MODEL_SYSTEM_PROMPT: added to the built-in prompt
	Intro          string // INTRO
	MaxConcurrency int    // MAX_CONCURRENCY: messages answered at once
	HistoryTurns   int    // HISTORY_TURNS: exchanges remembered per chat
	Version        string // the bot's version, set by main
	Test           *Test  // nil in production
}

// Test is for tests only, and nil in production. A field left zero keeps
// what production does.
type Test struct {
	ReloadGap  time.Duration // the least time between two loads of the network's contacts, instead of its own
	OpenRouter string        // the API host given OpenRouter's extras, instead of openrouter.ai
	Handled    func()        // called on the network's reader goroutine after each frame received
	busy       atomic.Int64  // work under way, as Begin counts it
}

// Begin counts work starting, a chat's worker or a contact load, until end
// is called. On a nil Test it does nothing.
func (t *Test) Begin() (end func()) {
	if t == nil {
		return func() {}
	}
	t.busy.Add(1)
	return func() { t.busy.Add(-1) }
}

// Idle reports whether no work Begin counted is under way.
func (t *Test) Idle() bool { return t.busy.Load() == 0 }

// Env is the setting key, trimmed, or def if it is unset or blank.
func Env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// number is the setting key as a positive number, else def.
func number(key string, def int) int {
	if n, err := strconv.Atoi(Env(key, "")); err == nil && n > 0 {
		return n
	}
	return def
}

// Load reads the core's settings.
func Load() *Config {
	c := &Config{
		APIURL:         strings.TrimRight(Env("MODEL_API_URL", ""), "/"),
		APIKey:         Env("MODEL_API_KEY", ""),
		Model:          Env("MODEL", "deepseek/deepseek-v4.1-flash-1017"),
		Reasoning:      strings.ToLower(Env("MODEL_REASONING", "low")),
		SystemPrompt:   Env("MODEL_SYSTEM_PROMPT", ""),
		Intro:          Env("INTRO", DefaultIntro),
		MaxConcurrency: number("MAX_CONCURRENCY", 32),
		HistoryTurns:   number("HISTORY_TURNS", 10),
	}
	// A key alone is OpenRouter's; a URL alone is a provider that needs no
	// key, such as a local one.
	c.NoModel = c.APIURL == "" && c.APIKey == ""
	if c.APIURL == "" {
		c.APIURL = "https://openrouter.ai/api/v1"
	}
	return c
}

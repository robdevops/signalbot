// Package config reads settings from the environment, the core's in Load,
// and holds the tests' hooks.
package config

import (
	"math"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/kit/txt"
)

// DefaultIntro is the self-introduction when INTRO is unset. {name},
// {model}, {tools} and {more} (features' lines) are filled in.
const DefaultIntro = "hi, I'm *{name}*, your open source assistant running {model}.\n\n" +
	"I respond to @{name} and replies to my messages.\n\n" +
	"Tools:\n{tools}\n\n{more}" +
	"https://github.com/robdevops/signalbot/blob/main/README.md"

// Config is the core's settings.
type Config struct {
	APIURL         string  // MODEL_API_URL: the chat completions API's base URL
	APIKey         string  // MODEL_API_KEY; "" sends none
	NoModel        bool    // neither MODEL_API_URL nor MODEL_API_KEY is set: only commands are answered
	Model          string  // MODEL: the model's ID at the API, such as "provider/model" on OpenRouter
	Reasoning      string  // MODEL_REASONING: the reasoning effort, "none" to leave it out
	SystemPrompt   string  // MODEL_SYSTEM_PROMPT: added to the built-in prompt
	Intro          string  // INTRO
	MaxConcurrency int     // MAX_CONCURRENCY: messages answered at once
	HistoryMax     int     // HISTORY_MAX: most messages remembered per chat; when full, the older half goes
	ToolLinger     int     // TOOL_LINGER_ROUNDS: replies a keyword tool group stays offered after one that matched or used it; 0 to HistoryMax
	DailyBudget    float64 // DAILY_BUDGET: US dollars each chat (a group or a DM) may spend a day; 0 for no cap
	StateDir       string  // STATE_DIRECTORY's first directory, for the core's saved state; "" for the working directory
	Version        string  // the bot's version, set by main
	Test           *Test   // nil in production
}

// Test is for tests only, and nil in production. A field left zero keeps
// what production does.
type Test struct {
	ReloadGap  time.Duration    // the least time between two loads of the network's contacts, instead of its own
	OpenRouter string           // the API host given OpenRouter's extras, instead of openrouter.ai
	Now        func() time.Time // the core's clock, instead of time.Now
	Handled    func()           // called on the network's reader goroutine after each frame received
	busy       atomic.Int64     // work under way, as Begin counts it
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
		HistoryMax:     number("HISTORY_MAX", 10),
		ToolLinger:     2,
	}
	// 0 is allowed: offered only on a message that matches. More than
	// HISTORY_MAX can't help: by then the exchange that used it has left
	// history, so the model doesn't know it was in use.
	if n, err := strconv.Atoi(strings.TrimSpace(Env("TOOL_LINGER_ROUNDS", ""))); err == nil && n >= 0 {
		c.ToolLinger = n
	}
	c.ToolLinger = min(c.ToolLinger, c.HistoryMax)
	c.StateDir, _, _ = strings.Cut(Env("STATE_DIRECTORY", ""), ":") // systemd may list several
	c.DailyBudget = 1
	if v := strings.TrimSpace(Env("DAILY_BUDGET", "")); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n >= 0 && !math.IsInf(n, 0) {
			c.DailyBudget = n
		} else {
			logx.Warnf("DAILY_BUDGET=%q is not a number of dollars, 0 or more; using 1", txt.Clamp(v, 20))
		}
	}
	// A key alone is OpenRouter's; a URL alone is a provider that needs no
	// key, such as a local one.
	c.NoModel = c.APIURL == "" && c.APIKey == ""
	if c.APIURL == "" {
		c.APIURL = "https://openrouter.ai/api/v1"
	}
	return c
}

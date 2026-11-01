// Package config reads the core's settings from the environment, and holds
// the tests' hooks. Every setting read, the core's or a feature's, is
// recorded for the docs test.
package config

import (
	"errors"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
)

// DefaultIntro is the self-introduction when INTRO is unset. {name},
// {model} and {tools} are filled in.
const DefaultIntro = "hi, I'm {name}, your open source assistant running {model} (tools: {tools}). " +
	"I respond to @mentions and replies to my messages. See " +
	"https://github.com/robdevops/signalbot/blob/main/README.md for details."

// Config is the core's settings.
type Config struct {
	Socket         string          // SIGNALCLI_SOCKET: a unix socket path, or host:port
	APIURL         string          // MODEL_API_URL: the chat completions API's base URL
	APIKey         string          // MODEL_API_KEY; "" sends none
	NoModel        bool            // neither MODEL_API_URL nor MODEL_API_KEY is set: only commands are answered
	Model          string          // MODEL: the model's ID at the API, such as "provider/model" on OpenRouter
	Reasoning      string          // MODEL_REASONING: the reasoning effort, "none" to leave it out
	SystemPrompt   string          // MODEL_SYSTEM_PROMPT: added to the built-in prompt
	Intro          string          // INTRO
	AllowedSenders map[string]bool // SIGNAL_DM_USER_IDS: who may DM the bot; changed in place by tests
	MaxConcurrency int             // MAX_CONCURRENCY: messages answered at once
	HistoryTurns   int             // HISTORY_TURNS: exchanges remembered per chat
	Version        string          // the bot's version, set by main
	Test           *Test           // nil in production
}

// Test is for tests only, and nil in production. A field left zero keeps
// what production does.
type Test struct {
	ReloadGap  time.Duration // the least time between two contact list loads, instead of 15s
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

// number is the setting key as a positive number, else def.
func number(key string, def int) int {
	if n, err := strconv.Atoi(Env(key, "")); err == nil && n > 0 {
		return n
	}
	return def
}

// Read is every setting read so far, sorted.
func Read() []string {
	mu.Lock()
	defer mu.Unlock()
	return slices.Sorted(maps.Keys(read))
}

// Load reads the core's settings.
func Load() (*Config, error) {
	c := &Config{
		Socket:         Env("SIGNALCLI_SOCKET", ""),
		APIURL:         strings.TrimRight(Env("MODEL_API_URL", ""), "/"),
		APIKey:         Env("MODEL_API_KEY", ""),
		Model:          Env("MODEL", "deepseek/deepseek-v4.1-flash-1017"),
		Reasoning:      strings.ToLower(Env("MODEL_REASONING", "low")),
		SystemPrompt:   Env("MODEL_SYSTEM_PROMPT", ""),
		Intro:          Env("INTRO", DefaultIntro),
		AllowedSenders: map[string]bool{},
		MaxConcurrency: number("MAX_CONCURRENCY", 32),
		HistoryTurns:   number("HISTORY_TURNS", 10),
	}
	// Split on commas, semicolons and any white space, so a CRLF env file or
	// a tab-separated list cannot leave an unmatchable ID in the list.
	for _, id := range strings.FieldsFunc(Env("SIGNAL_DM_USER_IDS", ""), func(r rune) bool {
		return r == ',' || r == ';' || unicode.IsSpace(r)
	}) {
		c.AllowedSenders[id] = true
	}
	if c.Socket == "" {
		return nil, errors.New("SIGNALCLI_SOCKET is required")
	}
	// A key alone is OpenRouter's; a URL alone is a provider that needs no
	// key, such as a local one.
	c.NoModel = c.APIURL == "" && c.APIKey == ""
	if c.APIURL == "" {
		c.APIURL = "https://openrouter.ai/api/v1"
	}
	return c, nil
}

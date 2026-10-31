package config

import (
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
)

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

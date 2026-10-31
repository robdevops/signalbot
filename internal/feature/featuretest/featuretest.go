// Package featuretest is a fake Host for testing one feature on its own.
package featuretest

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/feature"
)

// Host records what a feature registers, and lets a test run its hooks.
type Host struct {
	Ctx          context.Context
	Settings     map[string]string // what Env reads
	Capabilities feature.Caps
	Information  feature.Info
	Fetcher      func(ctx context.Context, c chat.ChatID, m chat.Media) ([]byte, error)

	mu          sync.Mutex
	Commands    map[string]func(context.Context, feature.Turn, string) (feature.Command, bool)
	Groups      []feature.ToolGroup
	Replies     []func(feature.Turn, feature.Reply) *chat.Outgoing
	Unaddressed []func(chat.Message)
	Forgets     []func(chat.ChatID)
	Remembered  []Remembered
}

// Remembered is one Remember call.
type Remembered struct {
	Chat    chat.ChatID
	Text    string
	Expires time.Duration
}

// New is a host with tools on and the given settings.
func New(settings map[string]string) *Host {
	return &Host{Ctx: context.Background(), Settings: settings, Capabilities: feature.Caps{Tools: true},
		Information: feature.Info{Model: "test-model", Tools: "none"}}
}

func (h *Host) Context() context.Context { return h.Ctx }
func (h *Host) Env() feature.Env         { return env(h.Settings) }
func (h *Host) Caps() feature.Caps       { return h.Capabilities }
func (h *Host) Info() feature.Info       { return h.Information }

func (h *Host) Command(name string, run func(context.Context, feature.Turn, string) (feature.Command, bool)) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.Commands == nil {
		h.Commands = map[string]func(context.Context, feature.Turn, string) (feature.Command, bool){}
	}
	if _, taken := h.Commands[name]; taken {
		return fmt.Errorf("the command /%s is taken", name)
	}
	h.Commands[name] = run
	return nil
}

func (h *Host) Tools(g feature.ToolGroup) { h.Groups = append(h.Groups, g) }

func (h *Host) OnReply(fn func(feature.Turn, feature.Reply) *chat.Outgoing) {
	h.Replies = append(h.Replies, fn)
}
func (h *Host) OnUnaddressed(fn func(chat.Message)) { h.Unaddressed = append(h.Unaddressed, fn) }
func (h *Host) OnForget(fn func(chat.ChatID))       { h.Forgets = append(h.Forgets, fn) }

func (h *Host) Remember(c chat.ChatID, text string, expires time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.Remembered = append(h.Remembered, Remembered{c, text, expires})
}

// env reads settings from a map, the way the bot reads the environment.
type env map[string]string

func (e env) String(name, def string) string {
	if v := strings.TrimSpace(e[name]); v != "" {
		return v
	}
	return def
}

func (e env) Bool(name string, def bool) bool {
	if b, err := strconv.ParseBool(e.String(name, "")); err == nil {
		return b
	}
	return def
}

func (e env) Int(name string, def int) int {
	if n, err := strconv.Atoi(e.String(name, "")); err == nil && n > 0 {
		return n
	}
	return def
}

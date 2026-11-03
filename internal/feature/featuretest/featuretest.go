// Package featuretest is a fake Host, so that a feature's tests run with no
// core and no other feature.
package featuretest

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/robdevops/signalbot/internal/feature"
)

// Host records what a feature registers and lets a test drive it. Set its
// exported fields before the feature registers.
type Host struct {
	Settings map[string]string                              // what Env reads
	Bot      feature.Info                                   // what Info returns
	Fetch    func(m feature.Message, i int) ([]byte, error) // what Media returns; nil fails
	Shared   []feature.Group                                // what Groups returns
	TimerErr error                                          // if set, At fails with it, as the core does at its cap
	Redactor func(string) string                            // what Redact does; nil leaves text as it is

	mu        sync.Mutex
	read      []string
	messages  []func(context.Context, feature.Message) feature.Action
	turns     []func(context.Context, feature.Message) feature.Additions
	groups    []feature.ToolGroup
	more      []string
	timers    []*Timer
	delivered []feature.Message
	life      context.Context // what Go passes; ended by Stop
	end       context.CancelFunc
	running   sync.WaitGroup
}

// Timer is a timer a feature set.
type Timer struct {
	Name string
	Due  time.Time
	Fn   func(ctx context.Context, due time.Time)
}

var _ feature.Host = (*Host)(nil)

// New is a Host with no settings.
func New() *Host { return &Host{Settings: map[string]string{}} }

func (h *Host) OnMessage(fn func(context.Context, feature.Message) feature.Action) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append(h.messages, fn)
}

func (h *Host) OnTurn(fn func(context.Context, feature.Message) feature.Additions) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.turns = append(h.turns, fn)
}

// IntroLine records an introduction line, ending " (DM only)" if dmOnly;
// IntroLines returns them.
func (h *Host) IntroLine(line string, dmOnly bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if dmOnly {
		line += " (DM only)"
	}
	h.more = append(h.more, line)
}

func (h *Host) IntroLines() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.more)
}

func (h *Host) Tools(g feature.ToolGroup) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.groups = append(h.groups, g)
}

func (h *Host) At(name string, t time.Time, fn func(context.Context, time.Time)) (func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.TimerErr != nil {
		return nil, h.TimerErr
	}
	tm := &Timer{name, t, fn}
	h.timers = append(h.timers, tm)
	return func() { h.remove(tm) }, nil
}

func (h *Host) remove(tm *Timer) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.timers = slices.DeleteFunc(h.timers, func(x *Timer) bool { return x == tm })
}

func (h *Host) Env(key, def string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.read = append(h.read, key)
	if v := strings.TrimSpace(h.Settings[key]); v != "" {
		return v
	}
	return def
}

func (h *Host) Info() feature.Info { return h.Bot }

func (h *Host) Media(_ context.Context, m feature.Message, i int) ([]byte, error) {
	if h.Fetch == nil {
		return nil, errors.New("no media")
	}
	return h.Fetch(m, i)
}

func (h *Host) Deliver(m feature.Message) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.delivered = append(h.delivered, m)
}

func (h *Host) Groups(context.Context, string) ([]feature.Group, error) { return h.Shared, nil }

// Redact runs Redactor, if set.
func (h *Host) Redact(text string) string {
	if h.Redactor == nil {
		return text
	}
	return h.Redactor(text)
}

// Go runs fn until Stop.
func (h *Host) Go(fn func(context.Context)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.life == nil {
		h.life, h.end = context.WithCancel(context.Background())
	}
	ctx := h.life
	h.running.Go(func() { fn(ctx) })
}

// Stop ends the ctx of what runs with Go, as the core does when the bot
// stops, and waits for it to return.
func (h *Host) Stop() {
	h.mu.Lock()
	end := h.end
	h.mu.Unlock()
	if end != nil {
		end()
	}
	h.running.Wait()
}

// Message runs the OnMessage hooks on m and returns the first Action that is
// non-zero, counting only what applies to m, as the core does.
func (h *Host) Message(ctx context.Context, m feature.Message) feature.Action {
	h.mu.Lock()
	hooks := slices.Clone(h.messages)
	h.mu.Unlock()
	for _, fn := range hooks {
		if a := fn(ctx, m).For(m); a != (feature.Action{}) {
			return a
		}
	}
	return feature.Action{}
}

// Turn runs the OnTurn hooks on m and returns what each added.
func (h *Host) Turn(ctx context.Context, m feature.Message) []feature.Additions {
	h.mu.Lock()
	hooks := slices.Clone(h.turns)
	h.mu.Unlock()
	var out []feature.Additions
	for _, fn := range hooks {
		out = append(out, fn(ctx, m))
	}
	return out
}

// Fire runs the earliest timer named name, as the core would when it is due,
// and reports whether there was one.
func (h *Host) Fire(ctx context.Context, name string) bool {
	h.mu.Lock()
	var next *Timer
	for _, tm := range h.timers {
		if tm.Name == name && (next == nil || tm.Due.Before(next.Due)) {
			next = tm
		}
	}
	h.mu.Unlock()
	if next == nil {
		return false
	}
	h.remove(next)
	next.Fn(ctx, next.Due)
	return true
}

// ToolGroups is the tool groups offered, in order.
func (h *Host) ToolGroups() []feature.ToolGroup {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.groups)
}

// Timers is the timers set and not yet run or cancelled.
func (h *Host) Timers() []Timer {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []Timer
	for _, tm := range h.timers {
		out = append(out, *tm)
	}
	return out
}

// Delivered is the messages passed to Deliver, in order.
func (h *Host) Delivered() []feature.Message {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.delivered)
}

// EnvRead is the settings read, in order.
func (h *Host) EnvRead() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.read)
}

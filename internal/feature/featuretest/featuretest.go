// Package featuretest is a fake Host, so that a feature's tests run with no
// core and no other feature.
package featuretest

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/state"
)

// Host records what a feature registers and lets a test drive it. Set its
// exported fields before the feature registers.
type Host struct {
	Settings map[string]string                                                                    // what Env reads
	Bot      feature.Info                                                                         // what Info returns
	Fetch    func(m feature.Message, i int) ([]byte, error)                                       // what Media returns; nil fails
	API      func(ctx context.Context, path string, body map[string]any) (json.RawMessage, error) // what CallAPI returns; nil fails
	Use      feature.Usage                                                                        // what Usage returns
	Shared   []feature.Group                                                                      // what Groups returns
	TimerErr error                                                                                // if set, At fails with it, as the core does at its cap
	Redactor func(string) string                                                                  // what Redact does; nil leaves text as it is
	StoreErr error                                                                                // if set, Store fails with it, as the core does with no usable state directory
	Unusable bool                                                                                 // if set, OfferTools drops a group with tools, as the core does when the API can run none
	Nets     func(feature.Chat) feature.Net                                                       // what Net returns; nil: Bot's network, name and caps, for every chat

	mu        sync.Mutex
	read      []string
	messages  []func(context.Context, feature.Message) feature.Action
	turns     []func(context.Context, feature.Message) feature.Additions
	events    []func(context.Context, feature.Event) string
	presses   []func(context.Context, feature.Press) (string, bool)
	groups    []feature.ToolGroup
	more      []string
	timers    []*Timer
	delivered []feature.Message
	posts     []feature.Post
	reactions []Reaction
	stores    []string
	charged   []feature.Chat
	commands  feature.Commands
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
func (h *Host) OnEvent(fn func(context.Context, feature.Event) string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, fn)
}

// Event runs the OnEvent hooks on e, as the core does, and returns the reply
// the core would send: the first non-empty one.
func (h *Host) Event(ctx context.Context, e feature.Event) string {
	h.mu.Lock()
	hooks := slices.Clone(h.events)
	h.mu.Unlock()
	for _, fn := range hooks {
		if r := fn(ctx, e); r != "" {
			return r
		}
	}
	return ""
}

func (h *Host) OnPress(fn func(context.Context, feature.Press) (string, bool)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.presses = append(h.presses, fn)
}

// Press runs the OnPress hooks on p, as the core does, and returns the
// answer of the first that takes it, and whether one did.
func (h *Host) Press(ctx context.Context, p feature.Press) (string, bool) {
	h.mu.Lock()
	hooks := slices.Clone(h.presses)
	h.mu.Unlock()
	for _, fn := range hooks {
		if a, ok := fn(ctx, p); ok {
			return a, true
		}
	}
	return "", false
}

// IntroLine records line; IntroLineDM records it marked " (DM only)".
func (h *Host) IntroLine(line string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.more = append(h.more, line)
}

func (h *Host) IntroLineDM(line string) { h.IntroLine(line + " (DM only)") }

func (h *Host) Command(name string, fn func(context.Context, feature.Message, string) feature.Action) error {
	if err := h.commands.Take(name); err != nil {
		return err
	}
	h.OnMessage(feature.CommandHook(name, fn))
	return nil
}

// ListCommands records a group with only its name, as the core keeps it.
func (h *Host) ListCommands(label string, names ...string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.groups = append(h.groups, feature.ToolGroup{Names: []string{"**" + label + "**: /" + strings.Join(names, ", /")}})
}

func (h *Host) IntroLines() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.more)
}

func (h *Host) OfferTools(g feature.ToolGroup) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.Unusable && len(g.Tools) > 0 {
		return false
	}
	h.groups = append(h.groups, g)
	return true
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

// Net is Nets' answer, else what Bot says.
func (h *Host) Net(chat feature.Chat) feature.Net {
	if h.Nets != nil {
		return h.Nets(chat)
	}
	intro := h.Bot.Intro
	if chat.GroupID == "" {
		intro = h.Bot.IntroDM
	}
	return feature.Net{Name: h.Bot.Network, Bot: h.Bot.Name, Caps: h.Bot.Caps, Intro: intro}
}

// Usage returns Use.
func (h *Host) Usage(context.Context, feature.Chat, bool) feature.Usage { return h.Use }

// CallAPI returns what API does; nil fails. It records the chat ctx is
// charged to, if any, for Charged.
func (h *Host) CallAPI(ctx context.Context, path string, body map[string]any) (json.RawMessage, error) {
	if chat, ok := feature.ChargedChat(ctx); ok {
		h.mu.Lock()
		h.charged = append(h.charged, chat)
		h.mu.Unlock()
	}
	if h.API == nil {
		return nil, errors.New("no API")
	}
	return h.API(ctx, path, body)
}

// Charged lists the chats CallAPI's calls were charged to, in order.
func (h *Host) Charged() []feature.Chat {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.charged)
}

// Store opens a saved state as the core does, in the directory
// Settings["STATE_DIRECTORY"] names (the working directory if unset): each
// name once, refusing the names the core refuses, and failing with StoreErr
// if set, which stands for a state directory the core can't use.
func (h *Host) Store(name string) (feature.Store, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.StoreErr != nil:
		return nil, h.StoreErr
	case state.CheckName(name) != nil:
		return nil, state.CheckName(name)
	case slices.Contains(h.stores, name):
		return nil, errors.New("a saved state called " + name + " is already open")
	}
	h.stores = append(h.stores, name)
	return state.Open(state.Dir(h.Settings["STATE_DIRECTORY"]), name), nil
}

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

// Post records p; Posts returns what was posted. OnSent is never called:
// a test calls it to say the post was sent or dropped.
func (h *Host) Post(p feature.Post) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.posts = append(h.posts, p)
}

// Posts is the posts passed to Post, in order.
func (h *Host) Posts() []feature.Post {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.posts)
}

// Reaction is a reaction passed to React.
type Reaction struct {
	Chat    feature.Chat
	Message feature.Message
	Emoji   string
}

func (h *Host) React(chat feature.Chat, msg feature.Message, emoji string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reactions = append(h.reactions, Reaction{chat, msg, emoji})
}

// Reactions is the reactions passed to React, in order.
func (h *Host) Reactions() []Reaction {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.reactions)
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
		if a := fn(ctx, m).For(m); !a.IsZero() {
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

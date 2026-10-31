package bot

import (
	"context"
	"fmt"
	"time"

	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/history"
)

type commandFunc = func(ctx context.Context, t feature.Turn, rest string) (feature.Command, bool)

// hooks is everything features registered, in registration order.
type hooks struct {
	commands    map[string]commandFunc
	groups      []feature.ToolGroup
	turn        []func(context.Context, feature.Turn) feature.Additions
	reply       []func(feature.Turn, feature.Reply) *chat.Outgoing
	unaddressed []func(chat.Message)
	forget      []func(chat.ChatID)
}

// Register registers the features, in order. Their order is the order of
// their tools in the prompt and of their hooks.
func (b *Bot) Register(features ...feature.Feature) error {
	for _, f := range features {
		if err := f.Register(host{b}); err != nil {
			return fmt.Errorf("%s: %w", f.Name(), err)
		}
	}
	return nil
}

// turn is the turn a message makes.
func (b *Bot) turn(m chat.Message, forced string) feature.Turn {
	return feature.Turn{Message: m, Bot: b.introName(m.Chat), Forced: forced}
}

// offer is the tool groups a turn offers, and their tools for the model; or
// only the forced tool. None if the model can't call tools.
func (b *Bot) offer(t feature.Turn) (groups []feature.ToolGroup, tools []feature.Tool) {
	if !b.cfg.Caps.Tools {
		return nil, nil
	}
	for _, g := range b.hooks.groups {
		if g.Offer == nil || g.Offer(t) {
			groups = append(groups, g)
		}
	}
	for _, g := range b.hooks.groups {
		for _, tl := range g.Tools {
			if t.Forced == "" && (g.Offer == nil || g.Offer(t)) || tl.Name == t.Forced {
				tools = append(tools, tl)
			}
		}
	}
	return groups, tools
}

// host is the Host a feature registers with.
type host struct{ b *Bot }

func (h host) Context() context.Context { return h.b.ctx }
func (h host) Env() feature.Env         { return h.b.cfg.Env }
func (h host) Caps() feature.Caps       { return h.b.cfg.Caps }
func (h host) Media() feature.Media     { return media{h.b} }

func (h host) Info() feature.Info {
	return feature.Info{Model: modelName(h.b.cfg.Model), Tools: h.b.toolsText()}
}

func (h host) Command(name string, run commandFunc) error {
	if _, taken := h.b.hooks.commands[name]; taken {
		return fmt.Errorf("the command /%s is taken", name)
	}
	h.b.hooks.commands[name] = run
	return nil
}

func (h host) Tools(g feature.ToolGroup) { h.b.hooks.groups = append(h.b.hooks.groups, g) }
func (h host) OnTurn(fn func(context.Context, feature.Turn) feature.Additions) {
	h.b.hooks.turn = append(h.b.hooks.turn, fn)
}
func (h host) OnReply(fn func(feature.Turn, feature.Reply) *chat.Outgoing) {
	h.b.hooks.reply = append(h.b.hooks.reply, fn)
}
func (h host) OnUnaddressed(fn func(chat.Message)) {
	h.b.hooks.unaddressed = append(h.b.hooks.unaddressed, fn)
}
func (h host) OnForget(fn func(chat.ChatID)) { h.b.hooks.forget = append(h.b.hooks.forget, fn) }

func (h host) Remember(c chat.ChatID, text string, expires time.Duration) {
	h.b.hist.Add(c, history.Entry{Role: "user", Content: text, Expires: history.Expiry(time.Now(), expires)})
}

// media fetches through the transport that owns the chat.
type media struct{ b *Bot }

func (m media) Fetch(ctx context.Context, c chat.ChatID, md chat.Media) ([]byte, error) {
	return m.b.tr(c).Fetch(ctx, c, md)
}

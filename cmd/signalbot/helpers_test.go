package main

import (
	"context"
	"github.com/robdevops/signalbot/internal/kit/img"
	"github.com/robdevops/signalbot/internal/kit/mcp/mcptest"
	"maps"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/chat"
	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/features/intro"
	"github.com/robdevops/signalbot/internal/llm"
	"github.com/robdevops/signalbot/internal/transports/signal"
)

// Config is a test's settings: the core's, and the features' (which the
// bot reads from the environment). The zero value has search, group
// context and vision off.
type Config struct {
	Model, ExtraPrompt, Intro    string
	Search, GroupContext, Vision bool
	NoTools, NoServerTools       bool
	MCPConfig                    string
	AllowedSenders               map[string]bool
	MaxConcurrency, HistoryTurns int
}

func testCfg() *Config {
	return &Config{Model: "m", AllowedSenders: map[string]bool{}, Intro: intro.Default, MaxConcurrency: 4, HistoryTurns: 2}
}

var defaultIntro = intro.Default

// adapters remembers each test bot's Signal adapter.
var adapters = map[*bot.Bot]*signal.Adapter{}

// typingEvery is how often test bots re-send typing.
var typingEvery = 50 * time.Millisecond

// NewBot builds the bot as main does: the core on the Signal adapter sig,
// with every feature, configured by cfg.
func NewBot(ctx context.Context, cfg *Config, ai llm.Completer, sig *signal.Adapter) *bot.Bot {
	os.Setenv("MODEL_SEARCH", strconv.FormatBool(cfg.Search))
	os.Setenv("GROUP_CONTEXT", strconv.FormatBool(cfg.GroupContext))
	os.Setenv("INTRO", cfg.Intro)
	os.Setenv("MCP_CONFIG", cfg.MCPConfig)
	b := bot.New(ctx, &bot.Config{Model: cfg.Model, ExtraPrompt: cfg.ExtraPrompt, MaxConcurrency: cfg.MaxConcurrency,
		HistoryTurns: cfg.HistoryTurns, Env: &config.Env{}, TypingEvery: typingEvery,
		Caps: feature.Caps{Vision: cfg.Vision, Tools: !cfg.NoTools, ServerTools: !cfg.NoServerTools}}, ai, sig)
	fs, _ := features(&config.Config{Model: cfg.Model})
	if err := b.Register(fs...); err != nil {
		panic(err)
	}
	adapters[b] = sig
	return b
}

// deliver hands env to the bot through its Signal adapter, as if read from
// the socket.
func deliver(b *bot.Bot, env *signal.Envelope) { adapters[b].Inject(env, b.Handle) }

func adapterOf(b *bot.Bot) *signal.Adapter { return adapters[b] }

// prompt is the system prompt for a Signal group message: one asking for a
// chart, or not.
func prompt(b *bot.Bot, chart bool) string {
	text := "hello"
	if chart {
		text = "chart it"
	}
	p, _ := b.SystemPrompt(chat.Message{Chat: chat.ChatID("signal:g:" + group), Group: true, Text: text})
	return p
}

// request is what the bot makes of an addressed message.
type request struct {
	chat.Message
	Kind, Prompt, line string
	Images             []chat.Media
	Sticker            *chat.Media
}

func (r *request) logLine() string { return r.line }

// decide is what the bot would answer of env, with DMs allowed from
// allowed, or nil if nothing.
func decide(env *signal.Envelope, allowed map[string]bool) *request {
	a := signal.NewAdapter("x", allowed)
	a.Client().SetSelf(self)
	var r *request
	a.Inject(env, func(in chat.Incoming) {
		if !in.Addressed {
			return
		}
		if m, ok := in.Load(context.Background()); ok {
			r = &request{Message: m, Kind: m.Why}
			r.Prompt, r.line = bot.UserTurn(m)
			for _, md := range m.Media {
				switch {
				case md.Kind == chat.Sticker:
					r.Sticker = &md
				case len(r.Images) < img.MaxPerMessage:
					r.Images = append(r.Images, md)
				}
			}
		}
	})
	return r
}

// mcpServers starts a fake MCP server for each name, offering the given
// tools, and writes an MCP_CONFIG for them, with chat_commands JSON for
// the servers in commands.
func mcpServers(t *testing.T, servers map[string][]string, commands map[string]string) string {
	t.Helper()
	var entries []string
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		srv := httptest.NewServer(&mcptest.Server{Tools: servers[name]})
		t.Cleanup(srv.Close)
		e := `"` + name + `": {"url": "` + srv.URL + `"`
		if c := commands[name]; c != "" {
			e += `, "chat_commands": ` + c
		}
		entries = append(entries, e+"}")
	}
	path := filepath.Join(t.TempDir(), "mcp.json")
	os.WriteFile(path, []byte(`{"mcpServers": {`+strings.Join(entries, ", ")+`}}`), 0o600)
	return path
}

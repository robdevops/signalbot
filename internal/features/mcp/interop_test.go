package mcp

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	ext "github.com/robdevops/signalbot/addon"
	"github.com/robdevops/signalbot/internal/feature/featuretest"
)

// TestPackageAddonProcess is not a real test: run as a subprocess with the
// argument "pkg", it is an addon built from the addon package, as an addon
// written in Go would be. /start answers, watches the chat for "guess N"
// and queues a post; the watch's tool reacts to each guess.
func TestPackageAddonProcess(t *testing.T) {
	if flag.Arg(0) != "pkg" {
		return
	}
	var s *ext.Server
	s = ext.New(ext.Config{Name: "game", Title: "Game", Version: "1", Tools: []ext.Tool{
		{
			Name: "start", Visibility: []string{"bot"},
			Call: func(_ context.Context, c *ext.Context, _ json.RawMessage) (ext.Result, error) {
				if c == nil {
					return ext.Text("no chat"), nil
				}
				if err := s.Queue(ext.Op{Op: "post", Chat: c.Chat, Text: "{" + c.Sender + "} started a round on " + s.Network()}); err != nil {
					return ext.Result{}, err
				}
				r := ext.Text("started")
				r.Meta = &ext.ResultMeta{Watch: &ext.Watch{ID: "r1", Pattern: `\bguess \d+`, Until: time.Now().Add(time.Hour).Format(time.RFC3339), Tool: "guess"}}
				return r, nil
			},
		},
		{
			Name: "guess", Visibility: []string{"bot"},
			Schema: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}}}`),
			Call: func(_ context.Context, _ *ext.Context, raw json.RawMessage) (ext.Result, error) {
				var a struct{ Text string }
				if err := ext.DecodeArgs(raw, &a); err != nil {
					return ext.Result{}, err
				}
				return ext.Result{Content: []ext.Content{{Type: "text", Text: "got " + a.Text}}, Meta: &ext.ResultMeta{React: "🎯"}}, nil
			},
		},
	}})
	if err := s.Run(context.Background(), os.Stdin, os.Stdout); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// The bot's side and the addon package, against each other: the handshake
// makes it an addon, a direct command answers and starts a watch, a
// matching message goes to the watch's tool and gets its reaction, and the
// post it queued reaches the chat with the placeholder filled in.
func TestPackageAddon(t *testing.T) {
	h, log := startAddon(t, `{"mcpServers":{"game":`+strings.Replace(addonServer("pkg", `{"chats":["`+groupA+`"],"post":true,"react":true,"watch":true}`,
		`,"chat_commands":{"start":{"tool":"start","direct":true}}`), "TestAddonProcess", "TestPackageAddonProcess", 1)+`}}`,
		func(h *featuretest.Host) { h.Bot.Network = "signal" })
	if !strings.Contains(log.String(), "<5>game MCP: an addon, with tools for the bot: guess, start\n") || !strings.Contains(log.String(), "<5>MCP server game: version 1\n") {
		t.Fatalf("log %q", log.String())
	}
	ctx := context.Background()
	if got := h.Message(ctx, in(inA, "/start")); got.Answer != "started" {
		t.Fatalf("/start: %+v", got)
	}
	eventually(t, "the post", func() bool { return len(h.Posts()) == 1 })
	if p := h.Posts()[0]; p.Chat != inA || p.Text != "@Alice started a round on signal" || p.Label != "Game" {
		t.Fatalf("post %+v", p)
	}
	if got := h.Message(ctx, said(inA, "bob-id", 1700000001000, "guess 42")); !got.Done {
		t.Fatalf("a guess wasn't taken: %+v", got)
	}
	eventually(t, "the reaction", func() bool { return len(h.Reactions()) == 1 })
	if r := h.Reactions()[0]; r.Emoji != "🎯" || r.Chat != inA || r.Message.Timestamp != 1700000001000 {
		t.Fatalf("reaction %+v", r)
	}
	if got := h.Message(ctx, said(inA, "bob-id", 1700000002000, "nice weather")); !got.IsZero() {
		t.Fatalf("chatter was taken: %+v", got)
	}
}

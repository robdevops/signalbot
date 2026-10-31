// Command signalbot is a chat bot for Signal that answers with a language
// model. This file wires the pieces together; features.go lists the
// features.
package main

import (
	"context"
	"fmt"
	"os"
	ossignal "os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/feature"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/llm"
	"github.com/robdevops/signalbot/internal/llm/openai"
	"github.com/robdevops/signalbot/internal/transports/signal"
)

// version is set at build time: go build -ldflags "-X main.version=v1.2.3"
var version = "dev"

// buildVersion is the -X version, else the version go build stamps from git.
func buildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && version == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println("signalbot", buildVersion())
		return
	}
	logx.Setup()
	logx.Infof("signalbot %s starting", buildVersion())
	env := &config.Env{}
	cfg, err := config.Load(env)
	if err != nil {
		logx.Errorf("config error: %v", err)
		os.Exit(1)
	}
	logx.SetLevel(cfg.LogLevel)
	sigCtx, stop := ossignal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The connection outlives the shutdown signal, so replies already being
	// written can still be sent; it closes once they finish or time runs out.
	connCtx, closeConn := context.WithCancel(context.Background())
	defer closeConn()

	model := openai.New(openai.Options{URL: cfg.APIURL, Key: cfg.APIKey, Model: cfg.Model, Reasoning: cfg.Reasoning})
	images, toolCalls := model.Capabilities(connCtx)
	search := env.Bool("MODEL_SEARCH", true)
	logx.Infof("model %s, reasoning %s, search %v, images %v, tools %v", cfg.Model, cfg.Reasoning, search, images, toolCalls)
	if !toolCalls {
		logx.Warnf("model %s can't call tools, so web search, charts and MCP tools are off", cfg.Model)
	}

	b := bot.New(connCtx, &bot.Config{Model: cfg.Model, ExtraPrompt: cfg.ExtraPrompt, MaxConcurrency: cfg.MaxConcurrency,
		HistoryTurns: cfg.HistoryTurns, Env: env,
		Caps: feature.Caps{Vision: images, Tools: toolCalls, ServerTools: model.OpenRouter()},
	}, llm.Loop{Model: model}, signal.NewAdapter(cfg.Socket, cfg.AllowedSenders))
	fs, waits := features(cfg)
	if err := b.Register(fs...); err != nil {
		logx.Errorf("config error: %v", err)
		os.Exit(1)
	}
	defer waits() // let local MCP servers stop after connCtx ends

	go func() {
		<-sigCtx.Done()
		stop() // a second signal now kills the process immediately
		logx.Infof("shutting down: finishing replies in flight")
		b.Shutdown(8 * time.Second) // under Docker's default 10s stop timeout
		closeConn()
	}()
	b.Run(connCtx)
	logx.Infof("stopped")
}

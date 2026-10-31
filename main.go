package main

import (
	"context"
	"fmt"
	"github.com/robdevops/signalbot/internal/llm"
	"github.com/robdevops/signalbot/internal/llm/openai"
	"os"
	ossignal "os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"github.com/robdevops/signalbot/internal/kit/logx"

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
	cfg, err := LoadConfig()
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

	sig := signal.NewAdapter(cfg.Socket, cfg.AllowedSenders)
	var tools *MCP
	if cfg.MCPConfig != "" {
		servers, err := loadMCPConfig(cfg.MCPConfig)
		if err == nil {
			tools, err = StartMCP(connCtx, servers)
		}
		if err != nil {
			logx.Errorf("config error: %v", err)
			os.Exit(1)
		}
		defer tools.Wait() // let stdio servers stop after connCtx ends
	}
	model := openai.New(openai.Options{URL: cfg.APIURL, Key: cfg.APIKey, Model: cfg.Model, Reasoning: cfg.Reasoning})
	bot := NewBot(connCtx, cfg, llm.Loop{Model: model}, sig)
	bot.mcp, bot.toolNames, bot.toolGroups = tools, tools.Names(), tools.ByServer()
	images, toolCalls := model.Capabilities(connCtx)
	bot.vision, bot.toolCalls, bot.serverTools = images, toolCalls, model.OpenRouter()
	if toolCalls {
		bot.commands = tools.Commands()
	} else if len(tools.Commands()) > 0 {
		logx.Warnf("model %s can't call tools, so chat commands are off", cfg.Model)
	}
	logx.Infof("model %s, reasoning %s, search %v, images %v, tools %v", cfg.Model, cfg.Reasoning, cfg.Search, images, toolCalls)
	if !toolCalls {
		logx.Warnf("model %s can't call tools, so web search, charts and MCP tools are off", cfg.Model)
	}
	if names := tools.Names(); len(names) > 0 {
		logx.Infof("MCP tools: %s", strings.Join(names, ", "))
	}
	if cmds := bot.commandNames(); len(cmds) > 0 {
		logx.Infof("chat commands: %s", strings.Join(cmds, ", "))
	}

	go func() {
		<-sigCtx.Done()
		stop() // a second signal now kills the process immediately
		logx.Infof("shutting down: finishing replies in flight")
		bot.Shutdown(8 * time.Second) // under Docker's default 10s stop timeout
		closeConn()
	}()
	bot.Run(connCtx)
	logx.Infof("stopped")
}

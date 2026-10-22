package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
)

// version is set at build time: go build -ldflags "-X main.version=v1.2.3"
var version = "dev"

func buildVersion() string {
	if version != "dev" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	var rev, dirty string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			if s.Value == "true" {
				dirty = "-dirty"
			}
		}
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	if rev == "" {
		return version
	}
	return "dev-" + rev + dirty
}

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println("signalbot", buildVersion())
		return
	}
	fmt.Println("signalbot", buildVersion())
	slog.Info("starting", "version", buildVersion())
	cfg, err := LoadConfig()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sig := NewSignal(cfg.Socket)
	bot := NewBot(cfg, sig, NewXAI(cfg))
	slog.Info("config", "model", cfg.Model, "reasoning", cfg.Reasoning, "search", cfg.Search, "allowed_senders", len(cfg.AllowedSenders))
	sig.Run(ctx, bot.Handle)
}

package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	cfg, err := LoadConfig()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	sig := NewSignal(cfg.Socket)
	bot := NewBot(cfg, sig, NewXAI(cfg))
	slog.Info("starting", "model", cfg.Model, "reasoning", cfg.Reasoning, "search", cfg.Search, "allowed_senders", len(cfg.AllowedSenders))
	sig.Run(ctx, bot.Handle)
}

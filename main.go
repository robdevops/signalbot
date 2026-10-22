package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"
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

// isTTY reports whether f is a terminal, i.e. we are running interactively
// rather than under systemd or Docker (where the log collector adds its own
// timestamp and ours would be noise).
func isTTY(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// setupLogging drops the date and time from log lines unless interactive.
// slog's default handler writes through the log package, so this keeps the
// familiar "INFO msg key=value" format.
func setupLogging(interactive bool) {
	if !interactive {
		log.SetFlags(0)
	}
}

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println("signalbot", buildVersion())
		return
	}
	interactive := isTTY(os.Stdout)
	setupLogging(interactive)
	fmt.Println("signalbot", buildVersion())
	slog.Info("starting", "version", buildVersion())
	cfg, err := LoadConfig()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The connection outlives the shutdown signal, so replies already being
	// written can still be sent; it closes once they finish or time runs out.
	connCtx, closeConn := context.WithCancel(context.Background())
	defer closeConn()

	sig := NewSignal(cfg.Socket)
	bot := NewBot(cfg, sig, NewXAI(cfg))
	bot.logTime = interactive
	bot.ctx = connCtx
	slog.Info("config", "model", cfg.Model, "reasoning", cfg.Reasoning, "search", cfg.Search,
		"allowed_senders", len(cfg.AllowedSenders), "log_message_body", cfg.LogMessageBody)

	go func() {
		<-sigCtx.Done()
		stop() // a second signal now kills the process immediately
		slog.Info("shutting down: finishing replies in flight")
		bot.Close()
		bot.Wait(8 * time.Second) // under Docker's default 10s stop timeout
		closeConn()
	}()
	sig.Run(connCtx, bot.Handle)
	slog.Info("stopped")
}

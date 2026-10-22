package main

import (
	"context"
	"fmt"
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

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "-version" || os.Args[1] == "--version") {
		fmt.Println("signalbot", buildVersion())
		return
	}
	logTimestamps = isTTY(os.Stdout)
	logf("signalbot %s starting", buildVersion())
	cfg, err := LoadConfig()
	if err != nil {
		logf("config error: %v", err)
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
	bot.ctx = connCtx
	logf("model %s, reasoning %s, search %v, %d allowed senders, log message body %v",
		cfg.Model, cfg.Reasoning, cfg.Search, len(cfg.AllowedSenders), cfg.LogMessageBody)

	go func() {
		<-sigCtx.Done()
		stop() // a second signal now kills the process immediately
		logf("shutting down: finishing replies in flight")
		bot.Close()
		bot.Wait(8 * time.Second) // under Docker's default 10s stop timeout
		closeConn()
	}()
	sig.Run(connCtx, bot.Handle)
	logf("stopped")
}

package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"
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
	log.SetOutput(os.Stdout)
	if st, err := os.Stdout.Stat(); err != nil || st.Mode()&os.ModeCharDevice == 0 {
		log.SetFlags(0) // journald and Docker add their own timestamps
	}
	toJournal.Store(stdoutIsJournal())
	infof("signalbot %s starting", buildVersion())
	cfg, err := LoadConfig()
	if err != nil {
		errorf("config error: %v", err)
		os.Exit(1)
	}
	setLogLevel(cfg.LogLevel)
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// The connection outlives the shutdown signal, so replies already being
	// written can still be sent; it closes once they finish or time runs out.
	connCtx, closeConn := context.WithCancel(context.Background())
	defer closeConn()

	sig := NewSignal(cfg.Socket)
	bot := NewBot(connCtx, cfg, sig, NewOpenRouter(cfg))
	infof("model %s, reasoning %s, search %v, %d allowed senders",
		cfg.Model, cfg.Reasoning, cfg.Search, len(cfg.AllowedSenders))

	go func() {
		<-sigCtx.Done()
		stop() // a second signal now kills the process immediately
		infof("shutting down: finishing replies in flight")
		bot.Shutdown(8 * time.Second) // under Docker's default 10s stop timeout
		closeConn()
	}()
	sig.Run(connCtx, bot.Handle)
	infof("stopped")
}

// Command signalbot answers Signal messages with a model on OpenRouter.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/kit/logx"
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
	logx.Init()
	logx.Noticef("signalbot %s starting", buildVersion())
	cfg, err := config.Load()
	if err != nil {
		logx.Critf("config error: %v", err)
		os.Exit(1)
	}
	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Work outlives the shutdown signal, so replies being written can still
	// be sent; it ends once they finish or time runs out.
	ctx, closeConn := context.WithCancel(context.Background())
	defer closeConn()
	b := bot.New(ctx, cfg)
	for _, register := range features {
		if err := register(b); err != nil {
			logx.Critf("config error: %v", err)
			os.Exit(1)
		}
	}
	go func() {
		<-sigCtx.Done()
		stop() // a second signal now kills the process at once
		logx.Noticef("shutting down: finishing replies in flight")
		b.Shutdown(8 * time.Second) // under Docker's default 10s stop timeout
		closeConn()
	}()
	b.Run()
	logx.Noticef("stopped")
}

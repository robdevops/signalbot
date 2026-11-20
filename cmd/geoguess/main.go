// Command geoguess is GeoGuess, a GeoGuessr-style game for signalbot: the
// bot posts a street-level photo, players answer with a location, and when
// time is up the bot names the place and ranks players by how far off
// they were.
//
// It is an MCP server on stdio (protocol 2025-06-18, newline-delimited
// JSON-RPC) that speaks signalbot's addon extension: signalbot starts it as
// a child process, calls its tools for /geo and for each guess its watch
// catches, and reads its outbox for the results. Stdout carries nothing
// but the protocol. It keeps its state and its own log in GEO_STATE_DIR;
// the log never holds a guess, a coordinate, a name or message text.
//
// Every setting is a GEO_ variable, which signalbot passes to an addon
// server called geo; see README.md.
package main

import (
	"context"
	"fmt"
	"log"
	"math/rand/v2"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"

	"github.com/robdevops/signalbot/cmd/geoguess/geo"
	"github.com/robdevops/signalbot/internal/kit/httpx"
)

// maxLog is the size past which the log is moved aside at start, keeping
// one old log.
const maxLog = 4 << 20

func main() { os.Exit(run()) }

func run() int {
	// Stdout is the protocol's: anything else printed by mistake goes to
	// stderr, which signalbot counts but never logs.
	stdout := os.Stdout
	os.Stdout = os.Stderr
	cfg, err := loadConfig(os.Getenv)
	if err == nil {
		err = checkDir(cfg.StateDir)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "geoguess: %v\n", err)
		return 1
	}
	debug.SetMemoryLimit(cfg.MemLimit)
	logger, closeLog, err := openLog(cfg.StateDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "geoguess: %v\n", err)
		return 1
	}
	defer closeLog()
	if cfg.Token == "" && cfg.Regions == "" {
		// Panoramax alone is mostly France: world seeds would mostly search
		// empty boxes.
		cfg.Regions = "FR"
		logger.Printf("GEO_MAPILLARY_TOKEN is not set: Panoramax only, in France unless GEO_REGIONS says otherwise")
	}
	client := httpx.Client(4)
	var sources []geo.Source
	if cfg.Token != "" {
		sources = append(sources, geo.NewMapillary(cfg.Mapillary, cfg.Token, client))
	}
	sources = append(sources, geo.NewPanoramax(cfg.Panoramax, client))
	finder := &search{
		sources: sources, rules: geo.Rules{MinQuality: cfg.MinQuality, MaxAge: cfg.MaxAge},
		places: geo.NewNominatim(cfg.Nominatim, client), rand: rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())),
		log: logger, seeds: map[string]*geo.Seeds{}, regions: cfg.Regions,
	}
	s, err := newServer(cfg, realClock{}, finder, logger)
	if err != nil {
		logger.Printf("can't start: %v", err)
		fmt.Fprintf(os.Stderr, "geoguess: %v\n", err)
		return 1
	}
	finder.used = s.isUsed

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go s.prefetch(ctx)
	done := make(chan error, 1)
	go func() { done <- s.serve(ctx, os.Stdin, stdout) }()
	select {
	case err = <-done:
	case <-ctx.Done():
	}
	s.save()
	if err != nil {
		logger.Printf("stopped: %v", err)
		return 1
	}
	logger.Printf("stopped")
	return 0
}

// openLog opens GeoGuess's own log, geoguess.log in dir, moving a large one
// aside to geoguess.log.1 first.
func openLog(dir string) (*log.Logger, func(), error) {
	path := filepath.Join(dir, "geoguess.log")
	if st, err := os.Stat(path); err == nil && st.Size() > maxLog {
		os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("can't open its log in GEO_STATE_DIR: %v", err)
	}
	return log.New(f, "", log.LstdFlags), func() { f.Close() }, nil
}

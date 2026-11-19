//go:build !nosignal

package main

import (
	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/signal"
)

// Signal, through signal-cli; -tags nosignal leaves it out.
func init() {
	networks["signal"] = func(t *config.Test) bot.Network { return signal.New(t) }
}

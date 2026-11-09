//go:build !nosignal

package main

import (
	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/signal"
)

// newNetwork is the chat network the bot answers on: Signal, through
// signal-cli. -tags nosignal leaves it out. With a second network, this
// becomes a choice by setting, and each network a file of its own.
func newNetwork(t *config.Test) bot.Network { return signal.New(t) }

//go:build !nodiscord

package main

import (
	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/discord"
)

// Discord, by its gateway and REST API; -tags nodiscord leaves it out.
func init() {
	networks["discord"] = func(t *config.Test) bot.Network { return discord.New(t) }
}

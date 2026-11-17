//go:build !notelegram

package main

import (
	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/telegram"
)

// Telegram, by its Bot API's long polling; -tags notelegram leaves it out.
func init() {
	networks["telegram"] = func(t *config.Test) bot.Network { return telegram.New(t) }
}

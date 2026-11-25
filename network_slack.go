//go:build !noslack

package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/slack"
)

// Slack, by Socket Mode; -tags noslack leaves it out. -slack-manifest
// prints the app manifest the bot last wrote, with its slash commands, from
// STATE_DIRECTORY, or without it from the systemd unit's state directory,
// Docker's /data or the working directory, and says which file it read.
func init() {
	networks["slack"] = func(t *config.Test) bot.Network { return slack.New(t) }
	networkChecks["slack"] = slack.Check
	networkFlags["-slack-manifest"] = func() {
		data, path, tried := slack.SavedManifest()
		os.Stdout.Write(data)
		if path != "" {
			fmt.Fprintf(os.Stderr, "signalbot: printed %s\n", path)
			return
		}
		fmt.Fprintf(os.Stderr, "signalbot: found no %s (tried %s), so this manifest has no slash commands: the bot writes it in its state directory at its first start on Slack; set STATE_DIRECTORY to read another directory\n",
			slack.ManifestFile, strings.Join(tried, ", "))
	}
}

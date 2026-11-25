//go:build !noslack

package main

import (
	"fmt"
	"os"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/slack"
)

// Slack, by Socket Mode; -tags noslack leaves it out. -slack-manifest
// prints the app manifest the bot last wrote, with its slash commands.
func init() {
	networks["slack"] = func(t *config.Test) bot.Network { return slack.New(t) }
	networkChecks["slack"] = slack.Check
	networkFlags["-slack-manifest"] = func() {
		data, saved := slack.SavedManifest()
		os.Stdout.Write(data)
		if !saved {
			fmt.Fprintf(os.Stderr, "signalbot: no %s in the state directory yet, so this manifest has no slash commands: start the bot once on Slack to write it\n", slack.ManifestFile)
		}
	}
}

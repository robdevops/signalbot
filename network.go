package main

import (
	"fmt"
	"strings"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/config"
)

// networks are the chat networks this build has, by name, each registered
// by its own network_<name>.go, which a build tag can leave out.
var networks = map[string]func(*config.Test) bot.Network{}

// The setting that chooses each network, and the tag that leaves it out.
var networkSettings = []struct{ name, setting, tag string }{
	{"telegram", "TELEGRAM_BOT_TOKEN", "notelegram"},
	{"signal", "SIGNALCLI_SOCKET", "nosignal"},
}

// newNetwork is the chat network the bot answers on: the one whose setting
// is set, else Signal, whose socket has a default. One network a process:
// both set is an error, as is a network this build leaves out.
func newNetwork(t *config.Test) (bot.Network, error) {
	name, err := chooseNetwork(func(k string) string { return config.Env(k, "") })
	if err != nil {
		return nil, err
	}
	return networks[name](t), nil
}

// chooseNetwork picks the network by which settings env has, among those
// this build has.
func chooseNetwork(env func(string) string) (string, error) {
	var set, settings []string
	for _, n := range networkSettings {
		if env(n.setting) != "" {
			set, settings = append(set, n.name), append(settings, n.setting)
		}
	}
	switch {
	case len(set) > 1:
		all := "both"
		if len(settings) > 2 {
			all = "all"
		}
		return "", fmt.Errorf("%s and %s are %s set: a bot answers on one network, so set one (run a bot for each, each with its own STATE_DIRECTORY)",
			strings.Join(settings[:len(settings)-1], ", "), settings[len(settings)-1], all)
	case len(set) == 0:
		set = []string{"signal"}
	}
	if _, ok := networks[set[0]]; !ok {
		for _, n := range networkSettings {
			if n.name == set[0] {
				return "", fmt.Errorf("%s is chosen, but this build leaves it out (built with -tags %s)", set[0], n.tag)
			}
		}
	}
	return set[0], nil
}

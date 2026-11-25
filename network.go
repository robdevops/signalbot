package main

import (
	"fmt"
	"strings"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

// networks are the chat networks this build has, by name, each registered
// by its own network_<name>.go, which a build tag can leave out.
var networks = map[string]func(*config.Test) bot.Network{}

// networkChecks report, for a network that has them, settings it can't
// start without, before any network is made.
var networkChecks = map[string]func() error{}

// networkFlags are a network's own command-line flags, each a chore that
// runs instead of the bot, such as -slack-manifest.
var networkFlags = map[string]func(){}

// The setting that chooses each network, and the tag that leaves it out.
var networkSettings = []struct{ name, setting, tag string }{
	{"signal", "SIGNALCLI_SOCKET", "nosignal"},
	{"telegram", "TELEGRAM_BOT_TOKEN", "notelegram"},
	{"slack", "SLACK_BOT_TOKEN", "noslack"},
}

// newNetwork is the chat networks the bot answers on, joined: those whose
// settings are set, else Signal, whose socket has a default. A network
// this build leaves out is an error.
func newNetwork(t *config.Test) (bot.Network, error) {
	names, err := chooseNetwork(func(k string) string { return config.Env(k, "") })
	if err != nil {
		return nil, err
	}
	for _, name := range names {
		if check := networkChecks[name]; check != nil {
			if err := check(); err != nil {
				return nil, err
			}
		}
	}
	var nets []bot.Network
	for _, name := range names {
		nets = append(nets, networks[name](t))
	}
	if len(nets) > 1 {
		logx.Noticef("on %s at once", strings.Join(names, " and "))
	}
	return bot.Join(nets...), nil
}

// chooseNetwork picks the networks by which settings env has, among those
// this build has, in networkSettings' order.
func chooseNetwork(env func(string) string) ([]string, error) {
	var set []string
	for _, n := range networkSettings {
		if env(n.setting) != "" {
			set = append(set, n.name)
		}
	}
	if len(set) == 0 {
		set = []string{"signal"}
	}
	for _, name := range set {
		if _, ok := networks[name]; !ok {
			for _, n := range networkSettings {
				if n.name == name {
					return nil, fmt.Errorf("%s is chosen, but this build leaves it out (built with -tags %s)", name, n.tag)
				}
			}
		}
	}
	return set, nil
}

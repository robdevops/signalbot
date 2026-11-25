package main

import (
	"strings"
	"testing"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/config"

	"github.com/robdevops/signalbot/internal/feature/featuretest"
	"github.com/robdevops/signalbot/internal/kit/logx"
)

// Every feature registers without error.
func TestFeaturesRegister(t *testing.T) {
	var logs logx.Buffer
	defer logx.SetOutput(&logs, &logs, false)()
	h := featuretest.New()
	for _, register := range enabled() {
		if err := register(h); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBuildVersion(t *testing.T) {
	if v := buildVersion(); v == "" {
		t.Fatal("no version")
	}
	version = "v1.2.3"
	defer func() { version = "dev" }()
	if v := buildVersion(); v != "v1.2.3" {
		t.Fatalf("version %q", v)
	}
}

// The networks are those whose settings are set, Signal first, else
// Signal; one this build leaves out is an error that says what to do.
func TestChooseNetwork(t *testing.T) {
	saved := networks
	defer func() { networks = saved }()
	both := map[string]func(*config.Test) bot.Network{"signal": nil, "telegram": nil}
	for _, tc := range []struct {
		name  string
		built map[string]func(*config.Test) bot.Network
		env   map[string]string
		want  string // the networks, joined by ","
		err   string
	}{
		{"neither set", both, nil, "signal", ""},
		{"socket set", both, map[string]string{"SIGNALCLI_SOCKET": "/run/s"}, "signal", ""},
		{"token set", both, map[string]string{"TELEGRAM_BOT_TOKEN": "x"}, "telegram", ""},
		{"both set", both, map[string]string{"TELEGRAM_BOT_TOKEN": "x", "SIGNALCLI_SOCKET": "/run/s"}, "signal,telegram", ""},
		{"left out", map[string]func(*config.Test) bot.Network{"signal": nil}, map[string]string{"TELEGRAM_BOT_TOKEN": "x"}, "", "telegram is chosen, but this build leaves it out (built with -tags notelegram)"},
		{"both, one left out", map[string]func(*config.Test) bot.Network{"signal": nil}, map[string]string{"TELEGRAM_BOT_TOKEN": "x", "SIGNALCLI_SOCKET": "/run/s"}, "", "telegram is chosen, but this build leaves it out"},
		{"default left out", map[string]func(*config.Test) bot.Network{"telegram": nil}, nil, "", "signal is chosen, but this build leaves it out (built with -tags nosignal)"},
		{"slack set", map[string]func(*config.Test) bot.Network{"signal": nil, "slack": nil}, map[string]string{"SLACK_BOT_TOKEN": "x"}, "slack", ""},
		{"all set", map[string]func(*config.Test) bot.Network{"signal": nil, "telegram": nil, "slack": nil},
			map[string]string{"SLACK_BOT_TOKEN": "x", "TELEGRAM_BOT_TOKEN": "x", "SIGNALCLI_SOCKET": "/run/s"}, "signal,telegram,slack", ""},
		{"slack left out", both, map[string]string{"SLACK_BOT_TOKEN": "x"}, "", "slack is chosen, but this build leaves it out (built with -tags noslack)"},
	} {
		networks = tc.built
		names, err := chooseNetwork(func(k string) string { return tc.env[k] })
		if got := strings.Join(names, ","); got != tc.want || (err == nil) != (tc.err == "") || err != nil && !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%s: %q, %v", tc.name, got, err)
		}
	}
}

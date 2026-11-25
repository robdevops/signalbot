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

// The network is the one whose setting is set, else Signal; both set, or
// one this build leaves out, is an error that says what to do.
func TestChooseNetwork(t *testing.T) {
	saved := networks
	defer func() { networks = saved }()
	both := map[string]func(*config.Test) bot.Network{"signal": nil, "telegram": nil}
	for _, tc := range []struct {
		name  string
		built map[string]func(*config.Test) bot.Network
		env   map[string]string
		want  string
		err   string
	}{
		{"neither set", both, nil, "signal", ""},
		{"socket set", both, map[string]string{"SIGNALCLI_SOCKET": "/run/s"}, "signal", ""},
		{"token set", both, map[string]string{"TELEGRAM_BOT_TOKEN": "x"}, "telegram", ""},
		{"both set", both, map[string]string{"TELEGRAM_BOT_TOKEN": "x", "SIGNALCLI_SOCKET": "/run/s"}, "", "TELEGRAM_BOT_TOKEN and SIGNALCLI_SOCKET are both set"},
		{"left out", map[string]func(*config.Test) bot.Network{"signal": nil}, map[string]string{"TELEGRAM_BOT_TOKEN": "x"}, "", "telegram is chosen, but this build leaves it out (built with -tags notelegram)"},
		{"default left out", map[string]func(*config.Test) bot.Network{"telegram": nil}, nil, "", "signal is chosen, but this build leaves it out (built with -tags nosignal)"},
	} {
		networks = tc.built
		got, err := chooseNetwork(func(k string) string { return tc.env[k] })
		if got != tc.want || (err == nil) != (tc.err == "") || err != nil && !strings.Contains(err.Error(), tc.err) {
			t.Errorf("%s: %q, %v", tc.name, got, err)
		}
	}
}

package main

// startBot wires the bot to the scenario fakes, as main does. It is the only
// scenario file that names the bot's code, so it is the one rewritten for a
// new version of the bot.

import (
	"context"
	"net/url"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/signal"
)

// settingNames is every environment variable the bot reads; each scenario
// starts with all of them cleared.
var settingNames = []string{"SIGNALCLI_SOCKET", "MODEL_API_URL", "MODEL_API_KEY", "MODEL", "MODEL_SEARCH", "MODEL_REASONING",
	"SIGNAL_DM_USER_IDS", "MODEL_SYSTEM_PROMPT", "INTRO", "MAX_CONCURRENCY", "HISTORY_TURNS",
	"MCP_CONFIG", "GROUP_CONTEXT", "JOURNAL_STREAM"}

// startBot starts the bot in w, connected to its fakes, and returns once it
// has connected and loaded its contacts. Everything it started is drained
// and stopped when the test ends. Each step of that is its own cleanup, so a
// failed wait in one cannot skip the rest and leave the bot logging into the
// next test.
func startBot(t *testing.T, w world) *harness {
	h := newHarness(t, w)
	for _, k := range settingNames {
		t.Setenv(k, "")
	}
	env := h.env()
	env["SIGNALCLI_SOCKET"], env["MCP_CONFIG"] = h.signalSock, h.mcpConfig
	switch {
	case w.NoModel:
	case w.OtherProvider:
		env["MODEL_API_URL"] = h.modelURL
	default:
		env["MODEL_API_URL"], env["MODEL_API_KEY"] = h.modelURL, "test-key"
	}
	for k, v := range env {
		t.Setenv(k, v)
	}

	// Every log line goes to the harness, prefixed with its priority. The
	// test hooks make the fake model API OpenRouter's unless it is another
	// provider's, let a scenario see contacts reload, and say when the bot
	// has read a frame and when it is idle.
	t.Cleanup(logx.SetOutput(h, h, true)) // runs last
	test := &config.Test{ReloadGap: contactReloadGap, Handled: func() { h.handled <- struct{}{} }}
	if u, _ := url.Parse(h.modelURL); !w.OtherProvider {
		test.OpenRouter = u.Hostname()
	}

	// What main does.
	cfg := config.Load()
	cfg.Test = test
	net, err := signal.New(cfg.Test)
	if err != nil {
		t.Fatal(err)
	}
	ctx, closeConn := context.WithCancel(context.Background())
	t.Cleanup(closeConn)
	b := bot.New(ctx, cfg, net)
	for _, register := range features {
		if err := register(b); err != nil {
			t.Fatal(err)
		}
	}
	stopped := make(chan struct{})
	go func() { defer close(stopped); b.Run() }()

	h.settle = func() { h.waitFor("the bot to finish its work", test.Idle) }
	h.shutdown = func() { b.Shutdown(8 * time.Second) }
	t.Cleanup(func() {
		closeConn()
		select {
		case <-stopped:
		case <-time.After(waitLimit):
			t.Error("the bot did not stop")
		}
		// With the client stopped no contact reload starts; wait for one
		// under way, so that none logs after the test.
		h.waitFor("the contact list to load", test.Idle)
		h.checkNoSecretsLogged()
	})
	t.Cleanup(h.settle) // runs first

	h.waitLog("bot account")
	h.waitLog("loaded", "contacts")
	return h
}

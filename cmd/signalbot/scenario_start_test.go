package main

// startBot wires the bot to the scenario fakes, as main does. It is the only
// scenario file that names the bot's code, so it is the one rewritten for a
// new version of the bot.

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/bot"
	"github.com/robdevops/signalbot/internal/config"
	"github.com/robdevops/signalbot/internal/kit/logx"
	"github.com/robdevops/signalbot/internal/openrouter"
	"github.com/robdevops/signalbot/internal/signal"
)

// settingNames is every environment variable the bot reads; each scenario
// starts with all of them cleared.
var settingNames = []string{"SIGNALCLI_SOCKET", "OPENROUTER_API_KEY", "MODEL", "MODEL_SEARCH", "MODEL_REASONING",
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
	env["SIGNALCLI_SOCKET"], env["OPENROUTER_API_KEY"], env["MCP_CONFIG"] = h.signalSock, "test-key", h.mcpConfig
	for k, v := range env {
		t.Setenv(k, v)
	}

	// Every log line goes to the harness, prefixed with its priority. The
	// hooks point the bot at the fakes, and say when it has read a frame.
	restore := logx.SetOutput(h, h, true)
	url, gap := openrouter.BaseURL, signal.ReloadGap
	openrouter.BaseURL, signal.ReloadGap = h.modelURL, contactReloadGap // so a scenario can see contacts reload
	signal.Handled = func() { h.handled <- struct{}{} }
	t.Cleanup(func() { // runs last
		restore()
		openrouter.BaseURL, signal.ReloadGap, signal.Handled = url, gap, nil
	})

	// What main does.
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	ctx, closeConn := context.WithCancel(context.Background())
	t.Cleanup(closeConn)
	b := bot.New(ctx, cfg)
	for _, register := range features {
		if err := register(b); err != nil {
			t.Fatal(err)
		}
	}
	stopped := make(chan struct{})
	go func() { defer close(stopped); b.Run() }()

	var shutDown atomic.Bool // Shutdown holds every slot once it returns
	h.settle = func() {
		h.waitFor("the bot to finish its work", func() bool { return shutDown.Load() || b.Idle() })
	}
	h.shutdown = func() { b.Shutdown(8 * time.Second); shutDown.Store(true) }
	t.Cleanup(func() {
		closeConn()
		select {
		case <-stopped:
		case <-time.After(waitLimit):
			t.Error("the bot did not stop")
		}
		// With the client stopped no contact reload starts; wait for one
		// under way, so that none logs after the test.
		h.waitFor("the contact list to load", func() bool { return shutDown.Load() || b.Idle() })
		h.checkNoSecretsLogged()
	})
	t.Cleanup(h.settle) // runs first

	h.waitLog("bot account")
	h.waitLog("loaded", "contacts")
	return h
}

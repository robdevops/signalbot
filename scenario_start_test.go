package main

// startBot wires the bot at e40dc5f to the scenario fakes, as main does. It
// is the only scenario file that names the bot's code, so it is the one
// rewritten for a new version of the bot.

import (
	"context"
	"log"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// settingNames is every environment variable the bot reads; each scenario
// starts with all of them cleared.
var settingNames = []string{"SIGNALCLI_SOCKET", "OPENROUTER_API_KEY", "MODEL", "MODEL_SEARCH", "REASONING",
	"SIGNAL_DM_USER_IDS", "SYSTEM_PROMPT", "INTRO", "MAX_CONCURRENCY", "HISTORY_TURNS", "LOG_LEVEL",
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
	if env["LOG_LEVEL"] == "" {
		env["LOG_LEVEL"] = "debug" // as much as the bot can log, all checked for message text
	}
	for k, v := range env {
		t.Setenv(k, v)
	}

	// Every log line goes to the harness, prefixed with its priority.
	out, flags, errOut, errFlags := log.Writer(), log.Flags(), errLog.Writer(), errLog.Flags()
	journal, level := toJournal.Load(), maxLevel.Load()
	log.SetOutput(h)
	log.SetFlags(0)
	errLog.SetOutput(h)
	errLog.SetFlags(0)
	toJournal.Store(true)
	t.Cleanup(func() { // runs last
		log.SetOutput(out)
		log.SetFlags(flags)
		errLog.SetOutput(errOut)
		errLog.SetFlags(errFlags)
		toJournal.Store(journal)
		maxLevel.Store(level)
	})

	// What main does, with the fakes' addresses.
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	setLogLevel(cfg.LogLevel)
	connCtx, closeConn := context.WithCancel(context.Background())
	t.Cleanup(closeConn)
	sig := NewSignal(cfg.Socket)
	var tools *MCP
	if cfg.MCPConfig != "" {
		servers, err := loadMCPConfig(cfg.MCPConfig)
		if err == nil {
			tools, err = StartMCP(connCtx, servers)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { closeConn(); tools.Wait() })
	ai := NewOpenRouter(cfg, tools)
	ai.url, ai.modelsURL = h.modelURL+"/chat/completions", h.modelURL+"/models"
	bot := NewBot(connCtx, cfg, sig, ai)
	bot.names.minRefresh = contactReloadGap // so a scenario can see contacts reload
	bot.toolNames, bot.toolGroups = tools.Names(), tools.ByServer()
	images, toolCalls := ai.Capabilities(connCtx)
	bot.vision, bot.toolCalls, ai.noTools = images, toolCalls, !toolCalls
	if toolCalls {
		bot.commands = tools.Commands()
	} else if len(tools.Commands()) > 0 {
		warnf("model %s can't call tools, so chat commands are off", cfg.Model)
	}
	infof("model %s, reasoning %s, search %v, images %v, tools %v", cfg.Model, cfg.Reasoning, cfg.Search, images, toolCalls)
	if !toolCalls {
		warnf("model %s can't call tools, so web search, charts and MCP tools are off", cfg.Model)
	}
	if names := tools.Names(); len(names) > 0 {
		infof("MCP tools: %s", strings.Join(names, ", "))
	}
	if cmds := bot.commandNames(); len(cmds) > 0 {
		infof("chat commands: %s", strings.Join(cmds, ", "))
	}

	var shutDown atomic.Bool // Shutdown holds every slot once it returns
	h.settle = func() {
		h.waitFor("the bot to finish its work", func() bool { return shutDown.Load() || len(bot.sem) == 0 })
		bot.names.loading.Lock()
		bot.names.loading.Unlock() // only to wait for a contact reload under way
	}
	h.shutdown = func() { bot.Shutdown(8 * time.Second); shutDown.Store(true) }
	// drain makes any contact reload that is due a no-op, and waits for one
	// under way, so that none calls signal-cli or logs after the test.
	drain := func() {
		bot.closing.Store(true)
		bot.names.mu.Lock()
		bot.names.refreshedAt, bot.names.minRefresh = time.Now(), time.Hour
		bot.names.mu.Unlock()
		bot.names.loading.Lock()
		bot.names.loading.Unlock() // only to wait for a reload under way
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		sig.Run(connCtx, func(e *Envelope) {
			bot.Handle(e)
			h.handled <- struct{}{}
		})
	}()
	t.Cleanup(func() {
		closeConn()
		select {
		case <-stopped:
		case <-time.After(waitLimit):
			t.Error("the bot did not stop")
		}
	})
	t.Cleanup(func() { drain(); h.checkNoSecretsLogged() })
	t.Cleanup(h.settle) // runs first

	h.waitFor("the bot to connect", func() bool { return sig.SelfID() != "" })
	h.waitFor("the contact list to load", func() bool {
		bot.names.mu.Lock()
		defer bot.names.mu.Unlock()
		return !bot.names.loadedAt.IsZero()
	})
	return h
}

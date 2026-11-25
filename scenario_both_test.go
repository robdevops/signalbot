//go:build !nosignal && !notelegram

package main

// The bot on Signal and Telegram at once: one process, both fakes.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// With both networks set, one bot answers on each, each chat on its own
// network: a Signal DM on Signal, a Telegram DM and group on Telegram, and
// /intro there with the bot's Telegram name.
func TestScenarioBothNetworks(t *testing.T) {
	tg := &tgHarness{t: t, updates: make(chan string, 64), nextID: 100}
	api := httptest.NewServer(http.HandlerFunc(tg.serveAPI))
	t.Cleanup(api.Close)
	h := startBot(t, world{Env: map[string]string{"TELEGRAM_BOT_TOKEN": tgToken, "TELEGRAM_DM_USERS": tgAlice, "TELEGRAM_API_URL": api.URL}})
	h.waitLog("networks: signal, telegram")
	h.waitLog("Telegram: connected as @botty_bot")

	if s := h.ask(fromAlice("hello on Signal")); s.Recipient != idAlice || s.Message != "ok" {
		t.Fatalf("Signal: sent %+v", s)
	}
	tg.send("message", tgFromAlice+","+tgInDM+`,"text":"hello on Telegram"`)
	sent := tg.waitCalls("sendMessage", 1)
	if c := sent[0].Params; c["chat_id"] != tgAlice || c["text"] != "ok" {
		t.Fatalf("Telegram: sent %+v", c)
	}
	h.noMoreSends() // nothing for Telegram went to Signal

	tg.atBot("/intro")
	sent = tg.waitCalls("sendMessage", 2)
	if c := sent[1].Params; c["chat_id"] != tgGroup || !strings.Contains(c["text"].(string), "Botty") {
		t.Fatalf("Telegram group: sent %+v", c)
	}
	if s := h.ask(atBot("/intro")); s.GroupID != idGroup || !strings.HasPrefix(s.Message, "hi, I'm") {
		t.Fatalf("Signal group: sent %+v", s)
	}
	h.noMoreSends()
	for _, l := range h.logLines() {
		if strings.Contains(l.Text, "SECRET-test-token") {
			t.Fatalf("the Telegram token was logged: %q", l.Text)
		}
	}
}

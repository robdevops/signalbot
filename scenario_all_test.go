//go:build !nosignal && !notelegram && !nodiscord && !noslack

package main

// The bot on all four networks at once: one process, each network's fakes.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// With every network set, one bot answers on each, and each chat only on
// its own network: a DM on Signal, Telegram, Discord and Slack, each
// answered there and nowhere else.
func TestScenarioAllNetworks(t *testing.T) {
	tg := &tgHarness{t: t, updates: make(chan string, 64), nextID: 100}
	tgAPI := httptest.NewServer(http.HandlerFunc(tg.serveAPI))
	t.Cleanup(tgAPI.Close)
	dc := &dcHarness{t: t, events: make(chan string, 64), nextID: 1300000000000000000}
	dcAPI := httptest.NewServer(http.HandlerFunc(dc.serveAPI))
	t.Cleanup(dcAPI.Close)
	sl := &slHarness{t: t}
	slAPI := httptest.NewServer(http.HandlerFunc(sl.serveAPI))
	t.Cleanup(slAPI.Close)
	h := startBot(t, world{Env: map[string]string{
		"TELEGRAM_BOT_TOKEN": tgToken, "TELEGRAM_DM_USER_IDS": tgAlice, "TELEGRAM_API_URL": tgAPI.URL,
		"DISCORD_BOT_TOKEN": dcToken, "DISCORD_API_URL": dcAPI.URL,
		"SLACK_BOT_TOKEN": slToken, "SLACK_APP_TOKEN": slAppToken, "SLACK_API_URL": slAPI.URL,
	}})
	h.waitLog("on signal and telegram and discord and slack at once")
	h.waitLog("connected to Telegram as @botty_bot")
	h.waitLog("connected to Discord as botty")
	h.waitLog("connected to Slack as @botty")

	if s := h.ask(fromAlice("hello on Signal")); s.Recipient != idAlice || s.Message != "ok" {
		t.Fatalf("Signal: sent %+v", s)
	}
	tg.send("message", tgFromAlice+","+tgInDM+`,"text":"hello on Telegram"`)
	if c := tg.waitCalls("sendMessage", 1)[0].Params; c["chat_id"] != tgAlice || c["text"] != "ok" {
		t.Fatalf("Telegram: sent %+v", c)
	}
	dc.send("MESSAGE_CREATE", `{"id":"$ID","type":0,`+dcInDM+`,`+dcFromBob+`,"content":"hello on Discord"}`)
	if c := dc.waitCalls("POST /channels/"+dcDM+"/messages", 1)[0].Body; c["content"] != "ok" {
		t.Fatalf("Discord: sent %+v", c)
	}
	sl.message(slAlice, "D0ALICEDM1", "hello on Slack")
	if c := sl.waitCalls("chat.postMessage", 1)[0]; c.Params["channel"] != "D0ALICEDM1" || c.Params["text"] != "ok" {
		t.Fatalf("Slack: sent %+v", c.Params)
	}
	// Each answer went only where it was asked: one send on each network.
	h.noMoreSends()
	if n := len(tg.callsTo("sendMessage")); n != 1 {
		t.Errorf("%d Telegram sends", n)
	}
	if n := len(sl.callsTo("chat.postMessage")); n != 1 {
		t.Errorf("%d Slack sends", n)
	}
	posts := 0
	for _, r := range dc.routes() {
		if strings.HasPrefix(r, "POST /channels/") && strings.HasSuffix(r, "/messages") {
			posts++
		}
	}
	if posts != 1 {
		t.Errorf("%d Discord sends", posts)
	}
	for _, l := range h.logLines() {
		for _, secret := range []string{"SECRET-test-token", slToken, slAppToken} {
			if strings.Contains(l.Text, secret) {
				t.Fatalf("a token was logged: %q", l.Text)
			}
		}
	}
}

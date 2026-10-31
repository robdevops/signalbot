package main

import (
	"context"
	"github.com/robdevops/signalbot/internal/llm"
	"strings"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/kit/logx/logxtest"

	"github.com/robdevops/signalbot/internal/transports/signal"
)

const secretText = "my-secret-message-text"

// panicAI panics with the conversation in the panic value, which is what the
// Go runtime would print to the log if nothing recovered it.
type panicAI struct{}

func (panicAI) Complete(_ context.Context, r llm.Request) (llm.Completion, error) {
	panic("boom: " + r.Messages[len(r.Messages)-1].Content)
}

// A panic while answering must not reach the log (its value and stack trace
// can hold message text), must not kill the bot, and must free the slot.
func TestPanicWhileAnsweringIsContained(t *testing.T) {
	cfg := testCfg()
	sig := signal.NewAdapter("x", cfg.AllowedSenders)
	sig.Client().SetSelf(self)
	b := NewBot(context.Background(), cfg, panicAI{}, sig)
	logs := logxtest.Capture(t)

	deliver(b, parse(t, strings.Replace(groupMention, "what is up", secretText, 1)))
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), "internal error") {
		if time.Now().After(deadline) {
			t.Fatalf("panic was not recovered and reported: %q", logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if strings.Contains(logs.String(), secretText) {
		t.Fatalf("message text reached the log: %q", logs.String())
	}
	for b.Busy() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the concurrency slot was not released after a panic")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

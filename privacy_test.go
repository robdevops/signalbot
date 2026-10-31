package main

import (
	"context"
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

func (panicAI) Complete(ctx context.Context, system string, msgs []ChatMsg) (Completion, error) {
	panic("boom: " + msgs[len(msgs)-1].Content)
}

// A panic while answering must not reach the log (its value and stack trace
// can hold message text), must not kill the bot, and must free the slot.
func TestPanicWhileAnsweringIsContained(t *testing.T) {
	cfg := testCfg()
	sig := signal.New("x")
	sig.SetSelf(self)
	b := NewBot(context.Background(), cfg, sig, panicAI{})
	logs := logxtest.Capture(t)

	b.Handle(parse(t, strings.Replace(groupMention, "what is up", secretText, 1)))
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
	for len(b.sem) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the concurrency slot was not released after a panic")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// API error bodies can quote the request, so only the message is logged.
func TestAPIErrorTextKeepsOnlyTheMessage(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"openai style": {`{"error":{"message":"No auth credentials","code":401}}`, "No auth credentials"},
		"openrouter moderation": {`{"error":{"message":"Input flagged","code":403,"metadata":{"flagged_input":"` +
			secretText + `","reasons":["x"]}}}`, "Input flagged"},
		"html":  {`<html>502 Bad Gateway ` + secretText + `</html>`, "(unreadable error body)"},
		"empty": {`{}`, "(no message)"},
	} {
		got := apiErrorText([]byte(tc.body))
		if got != tc.want || strings.Contains(got, secretText) {
			t.Errorf("%s: got %q, want %q", name, got, tc.want)
		}
	}
}

// A reply with no text, or one that is not JSON, must not echo the body: a
// response can include the conversation.
func TestBadReplyBodiesAreNotEchoed(t *testing.T) {
	for _, body := range []string{
		`{"output":[],"instructions":"` + secretText + `"}`,
		`{"choices":[],"echo":"` + secretText + `"}`,
		`{"output": ` + secretText,
	} {
		if _, err := parseChatCompletion([]byte(body)); err == nil || strings.Contains(err.Error(), secretText) {
			t.Errorf("body %q: err = %v", body, err)
		}
	}
}

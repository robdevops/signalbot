package main

import (
	"context"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf is a log sink that is safe to read while the bot's goroutines write.
type syncBuf struct {
	mu sync.Mutex
	sb strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sb.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sb.String()
}

func captureLogsSafely(t *testing.T) *syncBuf {
	t.Helper()
	buf := &syncBuf{}
	w, flags := log.Writer(), log.Flags()
	log.SetOutput(buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(w); log.SetFlags(flags) })
	return buf
}

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
	sig := NewSignal("x")
	sig.selfID.Store(self)
	b := NewBot(context.Background(), cfg, sig, panicAI{})
	logs := captureLogsSafely(t)

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
		"x.ai style":   {`{"code":"Client specified an invalid argument","error":"Incorrect API key"}`, "Incorrect API key"},
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
		for _, parse := range []func([]byte) (Completion, error){parseOutput, parseChatCompletion} {
			if _, err := parse([]byte(body)); err == nil || strings.Contains(err.Error(), secretText) {
				t.Errorf("body %q: err = %v", body, err)
			}
		}
	}
}

// A malformed frame from signal-cli is reported without quoting it.
func TestBadFrameIsNotQuoted(t *testing.T) {
	logs := captureLogs(t)
	NewSignal("x").dispatch([]byte(`{"params": `+secretText+`}`), func(*Envelope) {})
	if logs.Len() == 0 || strings.Contains(logs.String(), secretText) {
		t.Fatalf("log = %q", logs.String())
	}
}

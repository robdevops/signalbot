package signal

import (
	"fmt"
	"strings"
	"testing"

	"github.com/robdevops/signalbot/internal/feature"
)

// A long reply is one message, as signal-cli sends what is past 2,000
// bytes as a long-text attachment; a huge one is capped with a warning; the
// attachments go with it; an empty reply or a failed send is logged.
func TestSendOneMessageAndCaps(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	c := newClient(f.sock, nil, nil)
	run(t, c, f)
	dm := feature.Chat{User: aliceID}
	c.Send(dm, strings.Repeat("z ", 6000), []feature.Picture{{Data: []byte("PNG")}}, nil)
	eventually(t, "a send", func() bool { return len(f.calls("send")) == 1 })
	if s := f.calls("send")[0]; s["attachments"] == nil || len([]rune(s["message"].(string))) != maxReply || !strings.HasSuffix(s["message"].(string), "[…truncated]") {
		t.Errorf("send %d chars, attachments %v", len([]rune(fmt.Sprint(s["message"]))), s["attachments"] != nil)
	}
	if !strings.Contains(logs.String(), "reply truncated from 12000 chars") {
		t.Errorf("logs %q", logs)
	}
	c.Send(dm, " ", nil, nil)
	c.Send(dm, "", []feature.Picture{{Data: []byte("PNG")}}, nil)
	eventually(t, "the pictures alone", func() bool { return len(f.calls("send")) == 2 })
	if s := f.calls("send")[1]; s["message"] != "" || s["attachments"] == nil {
		t.Errorf("pictures sent as %v", s)
	}
	newClient("x", nil, nil).Send(dm, "secret words", nil, nil)
	if out := logs.String(); !strings.Contains(out, "empty reply, nothing sent") || !strings.Contains(out, "send failed: not connected") || strings.Contains(out, "secret") {
		t.Errorf("logs %q", out)
	}
}

func TestPrepare(t *testing.T) {
	text := func(reply string) string { m, _ := prepare(reply, "Bot"); return string(m.text) }
	if got := text("@Bot hi @Alice"); got != "hi @Alice" {
		t.Errorf("got %q", got)
	}
	if got := text("I respond to @Bot and @Alice"); got != "I respond to @Bot and @Alice" {
		t.Errorf("a self-mention not at the start stays: %q", got)
	}
	if _, ok := prepare("  \n ", "Bot"); ok {
		t.Error("blank is something")
	}
	long := strings.Repeat("word ", 300) + "\n" + strings.Repeat("x", 1500)
	if got := text(long); got != strings.TrimSpace(long) {
		t.Errorf("a long reply changed: %d runes", len([]rune(got)))
	}
	if got := text(strings.Repeat("z ", 6000)); len([]rune(got)) != maxReply || !strings.HasSuffix(got, "[…truncated]") {
		t.Errorf("capped: %d runes", len([]rune(got)))
	}
}

// A picture's name is made safe for a file name, or left to the fallback.
func TestFileName(t *testing.T) {
	for in, want := range map[string]string{
		"NVDA vs AMD, 1 month":           "NVDA vs AMD 1 month",
		"  a/b\\c:d*e?f\"g<h>i|j;k\n l ": "abcdefghijk l",
		"/;:":                            "",
		strings.Repeat("x", 90):          strings.Repeat("x", 80),
	} {
		if got := fileName(in); got != want {
			t.Errorf("fileName(%q) = %q, want %q", in, got, want)
		}
	}
}

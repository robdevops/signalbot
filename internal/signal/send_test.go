package signal

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/robdevops/signalbot/internal/feature"
)

// A long reply is split, a huge one capped with a warning, the attachments
// go with the first message, and an empty reply or a failed send is logged.
func TestSendSplitsAndCaps(t *testing.T) {
	logs := captureLogs(t)
	f := newFake(t)
	c := newClient(f.sock, nil, nil)
	run(t, c, f)
	dm := feature.Chat{User: aliceID}
	c.Send(dm, strings.Repeat("z ", 3*maxChars), [][]byte{[]byte("PNG")}, nil)
	eventually(t, "four sends", func() bool { return len(f.calls("send")) == 4 })
	for i, s := range f.calls("send") {
		if (s["attachments"] != nil) != (i == 0) {
			t.Errorf("send %d attachments %v", i, s["attachments"])
		}
	}
	if !strings.Contains(logs.String(), "reply truncated from 12000 chars") {
		t.Errorf("logs %q", logs)
	}
	c.Send(dm, " ", nil, nil)
	c.Send(dm, "", [][]byte{[]byte("PNG")}, nil)
	eventually(t, "the pictures alone", func() bool { return len(f.calls("send")) == 5 })
	if s := f.calls("send")[4]; s["message"] != "" || s["attachments"] == nil {
		t.Errorf("pictures sent as %v", s)
	}
	newClient("x", nil, nil).Send(dm, "secret words", nil, nil)
	if out := logs.String(); !strings.Contains(out, "empty reply, nothing sent") || !strings.Contains(out, "send failed: not connected") || strings.Contains(out, "secret") {
		t.Errorf("logs %q", out)
	}
}

func TestMentions(t *testing.T) {
	known := map[string]string{"alice": "u-a", "alice smith": "u-as", "bob": "u-b", "bob_2": "u-b2", "ambiguous": ""}
	for _, tc := range []struct {
		text string
		want []string
	}{
		{"no mentions here", nil},
		{"Hi @Alice!", []string{"3:6:u-a"}},
		{"hi @alice", []string{"3:6:u-a"}},
		{"hi @Alice Smith, ok", []string{"3:12:u-as"}},
		{"hi @Alicia", nil},
		{"mail me@Alice", nil},
		{"\U0001F389 @Alice!", []string{"3:6:u-a"}},
		{"@Bob and @bob_2", []string{"0:4:u-b", "9:6:u-b2"}},
		{"hi @Ambiguous and @Bob", []string{"18:4:u-b"}},
	} {
		if got := mentions(tc.text, known); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%q: got %v, want %v", tc.text, got, tc.want)
		}
	}
	if got := mentions("@Alice", nil); got != nil {
		t.Errorf("no names: %v", got)
	}
}

func TestStripAt(t *testing.T) {
	for _, tc := range []struct {
		text, want string
		found      bool
	}{
		{"@Bot hi", "hi", true},
		{"hi @bot, ok", "hi , ok", true},
		{"hi @signalbot", "hi", true},
		{"ask @Bot and @Bot again", "ask and again", true},
		{"hi @Botty", "hi @Botty", false},
		{"mail me@Bot", "mail me@Bot", false},
	} {
		got, found := stripAt(tc.text, "Bot", "signalbot")
		if got != tc.want || found != tc.found {
			t.Errorf("%q: %q %v", tc.text, got, found)
		}
	}
}

func TestParts(t *testing.T) {
	if got := parts("@Bot hi @Alice", "Bot"); !reflect.DeepEqual(got, []string{"hi @Alice"}) {
		t.Errorf("got %q", got)
	}
	if got := parts("  ", "Bot"); got != nil {
		t.Errorf("blank: %q", got)
	}
	long := strings.Repeat("word ", 300) + "\n" + strings.Repeat("x", 1500)
	got := parts(long, "Bot")
	if len(got) != 2 || got[0] != strings.TrimSpace(strings.Repeat("word ", 300)) || got[1] != strings.Repeat("x", 1500) {
		t.Errorf("split at the line break: %d parts", len(got))
	}
	got = parts(strings.Repeat("y", 3*maxChars), "Bot")
	if len(got) != 3 || len([]rune(got[0])) != maxChars {
		t.Errorf("hard split: %d parts", len(got))
	}
	got = parts(strings.Repeat("z ", 3*maxChars), "Bot")
	total := 0
	for _, p := range got {
		total += len([]rune(p))
	}
	if len(got) != 4 || total > maxReply || !strings.HasSuffix(got[3], "[…truncated]") {
		t.Errorf("capped: %d parts, %d chars, ends %q", len(got), total, fmt.Sprint(got[len(got)-1][max(0, len(got[len(got)-1])-20):]))
	}
}

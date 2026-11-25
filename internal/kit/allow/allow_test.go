package allow

import (
	"strings"
	"testing"

	"github.com/robdevops/signalbot/internal/kit/logx"
)

func TestParse(t *testing.T) {
	admin := func() (bool, bool) { return true, false }
	mod := func() (bool, bool) { return false, true }
	neither := func() (bool, bool) { return false, false }
	for _, tc := range []struct {
		value, str                     string
		alice, bobAdmin, bobMod, carol bool
	}{
		{"", "admins and mods", false, true, true, false},
		{"all", "anyone", true, true, true, true},
		{"ALL, alice", "anyone", true, true, true, true},
		{"none", "no one", false, false, false, false},
		{"none, alice", "1 ID", true, false, false, false},
		{"alice;\r\n\tdave", "2 IDs", true, false, false, false},
		{"Admins alice", "admins and 1 ID", true, true, false, false},
		{"mods", "mods", false, false, true, false},
		{"admins mods alice dave", "admins, mods and 2 IDs", true, true, true, false},
	} {
		p := Parse(tc.value)
		if p.String() != tc.str {
			t.Errorf("%q: String %q", tc.value, p.String())
		}
		if got := p.Allows(neither, "alice-uuid", "alice"); got != tc.alice {
			t.Errorf("%q: Alice %v", tc.value, got)
		}
		if got := p.Allows(admin, "bob"); got != tc.bobAdmin {
			t.Errorf("%q: Bob as admin %v", tc.value, got)
		}
		if got := p.Allows(mod, "bob"); got != tc.bobMod {
			t.Errorf("%q: Bob as mod %v", tc.value, got)
		}
		if got := p.Allows(neither, "carol"); got != tc.carol {
			t.Errorf("%q: Carol %v", tc.value, got)
		}
	}
	asked := false
	ask := func() (bool, bool) { asked = true; return true, true }
	Parse("alice").Allows(ask, "alice")
	Parse("alice").Allows(ask, "bob")
	Parse("all").Allows(ask, "bob")
	if asked {
		t.Fatal("asked for a role when no role could decide")
	}
	if Parse("").Listed("") || Parse("x").Listed("") {
		t.Fatal("an empty ID listed")
	}
	if Parse("").Who() != "admins and mods" || Parse("mods").Who() != "mods" || Parse("1").Who() != "" {
		t.Fatal("Who")
	}
}

// An ID may be written with its network's prefix or without; a sender, as
// the bot writes it, matches either.
func TestNorm(t *testing.T) {
	p := Parse("signal:3b1f6c2e-7a4d-4e58-9c13-0d2a5b8e71f4 telegram:123456789 100000000000000002 slack:U0123ABCD W0123ABCD +61400000000")
	for _, sender := range []string{"3b1f6c2e-7a4d-4e58-9c13-0d2a5b8e71f4", "123456789", "discord:100000000000000002", "slack:U0123ABCD", "slack:W0123ABCD", "+61400000000"} {
		if !p.Listed(sender) {
			t.Errorf("%s not listed", sender)
		}
	}
	if p.Listed("discord:123456789") {
		t.Error("a Telegram ID taken for Discord's")
	}
}

// A refused DM is warned of once per sender, naming the setting.
func TestRefused(t *testing.T) {
	var buf logx.Buffer
	t.Cleanup(logx.SetOutput(&buf, &buf, false))
	r := Refused{Setting: "SIGNAL_DM_USERS"}
	r.Log("bob")
	r.Log("bob")
	r.Log("alice")
	want := "a DM from bob ignored: SIGNAL_DM_USERS doesn't allow it; add the ID to SIGNAL_DM_USERS to allow DMs from them\n"
	if got := buf.String(); strings.Count(got, want) != 1 || strings.Count(got, "a DM from") != 2 {
		t.Fatalf("log %q", got)
	}
}

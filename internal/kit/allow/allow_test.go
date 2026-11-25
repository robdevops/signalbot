package allow

import "testing"

func TestParse(t *testing.T) {
	admin := func() bool { return true }
	notAdmin := func() bool { return false }
	for _, tc := range []struct {
		value, str           string
		alice, bobAdmin, bob bool
	}{
		{"", "admins", false, true, false},
		{"all", "anyone", true, true, true},
		{"ALL, alice", "anyone", true, true, true},
		{"none", "no one", false, false, false},
		{"none, alice", "1 ID", true, false, false},
		{"alice;\r\n\tcarol", "2 IDs", true, false, false},
		{"Admins alice", "admins and 1 ID", true, true, false},
	} {
		p := Parse(tc.value)
		if p.String() != tc.str {
			t.Errorf("%q: String %q", tc.value, p.String())
		}
		if got := p.Allows(notAdmin, "alice-uuid", "alice"); got != tc.alice {
			t.Errorf("%q: Alice %v", tc.value, got)
		}
		if got := p.Allows(admin, "bob"); got != tc.bobAdmin {
			t.Errorf("%q: Bob as admin %v", tc.value, got)
		}
		if got := p.Allows(notAdmin, "bob"); got != tc.bob {
			t.Errorf("%q: Bob %v", tc.value, got)
		}
	}
	asked := false
	Parse("alice").Allows(func() bool { asked = true; return true }, "alice")
	Parse("alice").Allows(func() bool { asked = true; return true }, "bob")
	if asked {
		t.Fatal("asked whether an admin when admins may not, or already listed")
	}
	if Parse("").Listed("") || Parse("x").Listed("") {
		t.Fatal("an empty ID listed")
	}
}

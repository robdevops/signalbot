package signal

import (
	"encoding/json"
	"testing"
)

const contactsJSON = `[{"number":null,"uuid":"` + alice + `","username":"alice.user20","name":"","givenName":null,"familyName":null,"nickName":null,"isArchived":false,"profile":{"lastUpdateTimestamp":1792622246207,"givenName":"Alice","familyName":null,"about":"","hasAvatar":true}}]`

func TestContactDisplayName(t *testing.T) {
	var cs []Contact
	if err := json.Unmarshal([]byte(contactsJSON), &cs); err != nil {
		t.Fatal(err)
	}
	if got, rank := displayName(cs[0]); got != "Alice" || rank != rankGiven {
		t.Fatalf("profile givenName: %q %d", got, rank)
	}
	c := Contact{GivenName: "Albert", Username: "alice.1"}
	c.Profile.GivenName = "Alice"
	if got, _ := displayName(c); got != "Albert" {
		t.Fatalf("contact givenName should win, got %q", got)
	}
	if got, rank := displayName((Contact{Username: "alice.1"})); got != "alice.1" || rank != rankUsername {
		t.Fatalf("username fallback: %q %d", got, rank)
	}
	if got, rank := displayName((Contact{})); got != "" || rank != 0 {
		t.Fatalf("empty: %q %d", got, rank)
	}
}

func TestResolvePriority(t *testing.T) {
	n := NewNames()
	m := Mention{Name: alice, UUID: alice}
	if got := n.Resolve(m); got != "" {
		t.Fatalf("unknown id should resolve to nothing, not leak: %q", got)
	}
	if got := n.Resolve(Mention{Name: "FromMention", UUID: alice}); got != "FromMention" {
		t.Fatalf("mention name: %q", got)
	}
	// username is the last resort, below the mention's own name
	n.replaceContacts([]Contact{{UUID: alice, Username: "alice.user20"}})
	if got := n.Resolve(m); got != "alice.user20" {
		t.Fatalf("username fallback: %q", got)
	}
	if got := n.Resolve(Mention{Name: "FromMention", UUID: alice}); got != "FromMention" {
		t.Fatalf("mention name beats username: %q", got)
	}
	n.Learn(&Envelope{SourceUUID: alice, SourceName: "Alice Smith"})
	if got := n.Resolve(m); got != "Alice Smith" {
		t.Fatalf("sender name beats username: %q", got)
	}
	var cs []Contact
	json.Unmarshal([]byte(contactsJSON), &cs)
	n.replaceContacts(cs)
	if got := n.Resolve(m); got != "Alice" {
		t.Fatalf("givenName should win: %q", got)
	}
	n.Learn(&Envelope{SourceUUID: alice, SourceName: "Aimee S"})
	if got := n.Resolve(m); got != "Alice" {
		t.Fatalf("live name must not displace givenName: %q", got)
	}
}
func TestUnnamedMentionNotAtPrefixed(t *testing.T) {
	m := Mention{UUID: "u-1", Name: "u-1", Start: 3, Length: 1}
	got := stripMentions("hi \uFFFC!", []Mention{m}, self, NewNames())
	if got != "hi [unnamed user]!" {
		t.Fatalf("got %q", got)
	}
}

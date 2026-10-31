package main

import (
	"reflect"
	"testing"
)

func TestBuildMentions(t *testing.T) {
	known := map[string]string{"Alice": "u-alice", "Alice Smith": "u-as", "Zack": "u-d", "example_user1": "u-s"}
	tests := []struct {
		name, text string
		want       []string
	}{
		{"none", "no mentions here", nil},
		{"simple", "Hi @Alice!", []string{"3:6:u-alice"}},
		{"case insensitive", "hi @alice", []string{"3:6:u-alice"}},
		{"longest name wins", "hi @Alice Smith, ok", []string{"3:12:u-as"}},
		{"word boundary", "hi @Albert", nil},
		{"not preceded by word", "mail me@Alice", nil},
		{"emoji counts as two UTF-16 units", "\U0001F389 @Alice!", []string{"3:6:u-alice"}},
		{"multiple", "@Zack and @example_user1", []string{"0:5:u-d", "10:14:u-s"}},
		{"unknown name", "hi @Nobody", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildMentions(tc.text, known); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	if got := buildMentions("@Alice", nil); got != nil {
		t.Fatalf("nil known: %v", got)
	}
}

func TestDecideCollectsNamesForGroupsOnly(t *testing.T) {
	r := decide(parse(t, groupMention), nil)
	if r.Names["alice"] != alice {
		t.Fatalf("sender name missing: %v", r.Names)
	}
	if d := decide(parse(t, dm), map[string]bool{alice: true}); d.Names != nil {
		t.Fatalf("DMs should not collect names: %v", d.Names)
	}
}

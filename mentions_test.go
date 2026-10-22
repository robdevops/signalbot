package main

import (
	"reflect"
	"testing"
)

func TestBuildMentions(t *testing.T) {
	known := map[string]string{"Rob": "u-rob", "Rob Stevens": "u-rs", "Dkam": "u-d", "svs_fluffyegg": "u-s"}
	tests := []struct {
		name, text string
		want       []string
	}{
		{"none", "no mentions here", nil},
		{"simple", "Hi @Rob!", []string{"3:4:u-rob"}},
		{"case insensitive", "hi @rob", []string{"3:4:u-rob"}},
		{"longest name wins", "hi @Rob Stevens, ok", []string{"3:12:u-rs"}},
		{"word boundary", "hi @Robert", nil},
		{"not preceded by word", "mail me@Rob", nil},
		{"emoji counts as two UTF-16 units", "\U0001F389 @Rob!", []string{"3:4:u-rob"}},
		{"multiple", "@Dkam and @svs_fluffyegg", []string{"0:5:u-d", "10:14:u-s"}},
		{"unknown name", "hi @Nobody", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildMentions(tc.text, known); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
	if got := buildMentions("@Rob", nil); got != nil {
		t.Fatalf("nil known: %v", got)
	}
}

func TestDecideCollectsNamesForGroupsOnly(t *testing.T) {
	r := Decide(parse(t, groupMention), self, nil, nil)
	if r.Names["Rob"] != rob {
		t.Fatalf("sender name missing: %v", r.Names)
	}
	if d := Decide(parse(t, dm), self, map[string]bool{rob: true}, nil); d.Names != nil {
		t.Fatalf("DMs should not collect names: %v", d.Names)
	}
}

package main

import "testing"

func TestBuildVersion(t *testing.T) {
	old := version
	defer func() { version = old }()

	version = "v1.2.3"
	if got := buildVersion(); got != "v1.2.3" {
		t.Fatalf("got %q", got)
	}
	version = "dev"
	if got := buildVersion(); got == "" {
		t.Fatal("empty version")
	}
}

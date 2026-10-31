package main

import (
	"testing"

	"github.com/robdevops/signalbot/internal/feature/featuretest"
)

// Every feature registers without error.
func TestFeaturesRegister(t *testing.T) {
	h := featuretest.New()
	for _, register := range features {
		if err := register(h); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBuildVersion(t *testing.T) {
	if v := buildVersion(); v == "" {
		t.Fatal("no version")
	}
	version = "v1.2.3"
	defer func() { version = "dev" }()
	if v := buildVersion(); v != "v1.2.3" {
		t.Fatalf("version %q", v)
	}
}

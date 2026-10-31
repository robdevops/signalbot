package main

import (
	"testing"

	"github.com/robdevops/signalbot/internal/kit/logx"
)

func TestLogLevelConfig(t *testing.T) {
	t.Setenv("SIGNALCLI_SOCKET", "/x")
	t.Setenv("OPENROUTER_API_KEY", "k")
	for v, want := range map[string]logx.Level{"": logx.LevelInfo, "debug": logx.LevelDebug, "INFO": logx.LevelInfo, "warn": logx.LevelWarn, "warning": logx.LevelWarn, "error": logx.LevelError} {
		t.Setenv("LOG_LEVEL", v)
		c, err := LoadConfig()
		if err != nil || c.LogLevel != want {
			t.Errorf("LOG_LEVEL=%q: Level %d, err %v; want %d", v, c.LogLevel, err, want)
		}
	}
	t.Setenv("LOG_LEVEL", "verbose")
	if _, err := LoadConfig(); err == nil {
		t.Error("an unknown LOG_LEVEL must be a config error")
	}
}

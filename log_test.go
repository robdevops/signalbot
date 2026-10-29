package main

import (
	"strings"
	"testing"
)

// withLogMode sets the journald flag and level for one test.
func withLogMode(t *testing.T, journal bool, l level) {
	t.Helper()
	oldJ, oldL := toJournal.Load(), maxLevel.Load()
	toJournal.Store(journal)
	setLogLevel(l)
	t.Cleanup(func() { toJournal.Store(oldJ); maxLevel.Store(oldL) })
}

func logAll() {
	errorf("e")
	warnf("w")
	infof("i")
	debugf("d")
}

// Under systemd each line starts with its syslog priority, which journald
// stores as the entry's priority and journalctl colours by.
func TestJournalLinesCarryTheirPriority(t *testing.T) {
	withLogMode(t, true, levelDebug)
	logs := captureLogs(t)
	logAll()
	if got, want := logs.String(), "<3>e\n<4>w\n<6>i\n<7>d\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Elsewhere (a terminal, Docker) a word says the level; info lines stay plain.
func TestOtherLinesNameTheirLevel(t *testing.T) {
	withLogMode(t, false, levelDebug)
	logs := captureLogs(t)
	logAll()
	if got, want := logs.String(), "error: e\nwarning: w\ni\ndebug: d\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLogLevelFilters(t *testing.T) {
	for _, tc := range []struct {
		l    level
		want string
	}{
		{levelInfo, "<3>e\n<4>w\n<6>i\n"},
		{levelWarn, "<3>e\n<4>w\n"},
		{levelError, "<3>e\n"},
	} {
		withLogMode(t, true, tc.l)
		logs := captureLogs(t)
		logAll()
		if got := logs.String(); got != tc.want {
			t.Errorf("level %d: got %q, want %q", tc.l, got, tc.want)
		}
	}
}

// The priority prefix comes before the escaping, so a value cannot forge one
// on a new line.
func TestJournalPrefixCannotBeForged(t *testing.T) {
	withLogMode(t, true, levelInfo)
	logs := captureLogs(t)
	infof("name %s", "x\n<3>forged")
	if got := logs.String(); strings.Count(got, "\n") != 1 || !strings.HasPrefix(got, "<6>") {
		t.Fatalf("got %q", got)
	}
}

func TestLogLevelConfig(t *testing.T) {
	t.Setenv("SIGNALCLI_SOCKET", "/x")
	t.Setenv("OPENROUTER_API_KEY", "k")
	for v, want := range map[string]level{"": levelInfo, "debug": levelDebug, "INFO": levelInfo, "warn": levelWarn, "warning": levelWarn, "error": levelError} {
		t.Setenv("LOG_LEVEL", v)
		c, err := LoadConfig()
		if err != nil || c.LogLevel != want {
			t.Errorf("LOG_LEVEL=%q: level %d, err %v; want %d", v, c.LogLevel, err, want)
		}
	}
	t.Setenv("LOG_LEVEL", "verbose")
	if _, err := LoadConfig(); err == nil {
		t.Error("an unknown LOG_LEVEL must be a config error")
	}
}

// Without journald reading priorities, errors and warnings go to stderr and
// the rest to stdout, so Docker's journald driver can tell them apart.
func TestStreamsSplitWithoutJournal(t *testing.T) {
	withLogMode(t, false, levelDebug)
	var out, errOut strings.Builder
	redirectLogs(t, &out, &errOut)
	logAll()
	if got, want := errOut.String(), "error: e\nwarning: w\n"; got != want {
		t.Errorf("stderr: got %q, want %q", got, want)
	}
	if got, want := out.String(), "i\ndebug: d\n"; got != want {
		t.Errorf("stdout: got %q, want %q", got, want)
	}
}

// Under systemd everything goes to stdout: the prefix carries the priority.
func TestJournalUsesStdoutOnly(t *testing.T) {
	withLogMode(t, true, levelDebug)
	var out, errOut strings.Builder
	redirectLogs(t, &out, &errOut)
	logAll()
	if errOut.Len() != 0 || out.String() != "<3>e\n<4>w\n<6>i\n<7>d\n" {
		t.Fatalf("stdout %q, stderr %q", out.String(), errOut.String())
	}
}

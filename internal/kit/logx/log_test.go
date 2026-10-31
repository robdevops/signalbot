package logx

import (
	"bytes"
	"strings"
	"testing"
)

// withLogMode sets the journald flag and Level for one test.
func withLogMode(t *testing.T, journal bool, l Level) {
	t.Helper()
	oldJ, oldL := toJournal.Load(), maxLevel.Load()
	toJournal.Store(journal)
	SetLevel(l)
	t.Cleanup(func() { toJournal.Store(oldJ); maxLevel.Store(oldL) })
}

func logAll() {
	Errorf("e")
	Warnf("w")
	Infof("i")
	Debugf("d")
}

// Under systemd each line starts with its syslog priority, which journald
// stores as the entry's priority and journalctl colours by.
func TestJournalLinesCarryTheirPriority(t *testing.T) {
	withLogMode(t, true, LevelDebug)
	logs := captureLogs(t)
	logAll()
	if got, want := logs.String(), "<3>e\n<4>w\n<6>i\n<7>d\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Elsewhere (a terminal, Docker) a word says the level; info lines stay plain.
func TestOtherLinesNameTheirLevel(t *testing.T) {
	withLogMode(t, false, LevelDebug)
	logs := captureLogs(t)
	logAll()
	if got, want := logs.String(), "error: e\nwarning: w\ni\ndebug: d\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestLogLevelFilters(t *testing.T) {
	for _, tc := range []struct {
		l    Level
		want string
	}{
		{LevelInfo, "<3>e\n<4>w\n<6>i\n"},
		{LevelWarn, "<3>e\n<4>w\n"},
		{LevelError, "<3>e\n"},
	} {
		withLogMode(t, true, tc.l)
		logs := captureLogs(t)
		logAll()
		if got := logs.String(); got != tc.want {
			t.Errorf("Level %d: got %q, want %q", tc.l, got, tc.want)
		}
	}
}

// The priority prefix comes before the escaping, so a value cannot forge one
// on a new line.
func TestJournalPrefixCannotBeForged(t *testing.T) {
	withLogMode(t, true, LevelInfo)
	logs := captureLogs(t)
	Infof("name %s", "x\n<3>forged")
	if got := logs.String(); strings.Count(got, "\n") != 1 || !strings.HasPrefix(got, "<6>") {
		t.Fatalf("got %q", got)
	}
}

// Without journald reading priorities, errors and warnings go to stderr and
// the rest to stdout, so Docker's journald driver can tell them apart.
func TestStreamsSplitWithoutJournal(t *testing.T) {
	withLogMode(t, false, LevelDebug)
	var out, errOut strings.Builder
	t.Cleanup(Redirect(&out, &errOut))
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
	withLogMode(t, true, LevelDebug)
	var out, errOut strings.Builder
	t.Cleanup(Redirect(&out, &errOut))
	logAll()
	if errOut.Len() != 0 || out.String() != "<3>e\n<4>w\n<6>i\n<7>d\n" {
		t.Fatalf("stdout %q, stderr %q", out.String(), errOut.String())
	}
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	t.Cleanup(Redirect(&buf, &buf))
	return &buf
}

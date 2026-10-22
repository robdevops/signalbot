package main

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func logWith(timestamp, priority bool, f func(*slog.Logger)) string {
	var buf bytes.Buffer
	f(slog.New(newLogHandler(&buf, timestamp, priority)))
	return buf.String()
}

func TestLogHandlerPrintsNoLevel(t *testing.T) {
	out := logWith(false, false, func(l *slog.Logger) {
		l.Info("x.ai reply", "chat", "g:abc=", "duration", 1200*time.Millisecond, "tokens_in", 42,
			"cost_usd", "0.00012", "name", "Rob Stevens", "empty", "", "err", errors.New("boom"))
		l.Warn("careful")
		l.Error("broken", "err", errors.New("connection refused"))
	})
	want := `x.ai reply chat="g:abc=" duration=1.2s tokens_in=42 cost_usd=0.00012 name="Rob Stevens" empty="" err=boom` + "\n" +
		"careful\n" +
		`broken err="connection refused"` + "\n"
	if out != want {
		t.Fatalf("got\n%s\nwant\n%s", out, want)
	}
}

func TestLogHandlerTimestampOnlyWhenInteractive(t *testing.T) {
	stamp := regexp.MustCompile(`^\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2} starting\n$`)
	if out := logWith(true, false, func(l *slog.Logger) { l.Info("starting") }); !stamp.MatchString(out) {
		t.Fatalf("interactive: want a timestamp, got %q", out)
	}
	if out := logWith(false, false, func(l *slog.Logger) { l.Info("starting") }); out != "starting\n" {
		t.Fatalf("non-interactive: want no timestamp, got %q", out)
	}
}

func TestLogHandlerJournalPriority(t *testing.T) {
	out := logWith(false, true, func(l *slog.Logger) {
		l.Info("i")
		l.Warn("w")
		l.Error("e")
	})
	if out != "<6>i\n<4>w\n<3>e\n" {
		t.Fatalf("journal priorities: %q", out)
	}
	if out := logWith(false, false, func(l *slog.Logger) { l.Error("e") }); out != "e\n" {
		t.Fatalf("no priority prefix outside the journal: %q", out)
	}
	if out := logWith(false, false, func(l *slog.Logger) { l.Debug("d") }); out != "" {
		t.Fatalf("debug should be off: %q", out)
	}
}

func TestLogHandlerValuesCannotSplitLines(t *testing.T) {
	out := logWith(false, false, func(l *slog.Logger) {
		l.Info("msg\nforged", "k", "v\n<3>forged error", "tab", "a\tb")
	})
	if strings.Count(out, "\n") != 1 {
		t.Fatalf("a message or value split the line: %q", out)
	}
	if strings.Contains(out, "\n<3>") {
		t.Fatalf("a value forged a journal priority: %q", out)
	}
}

func TestLogHandlerWithAttrsAndGroup(t *testing.T) {
	out := logWith(false, false, func(l *slog.Logger) {
		l.With("svc", "bot").WithGroup("req").Info("m", "id", 7, slog.Group("u", "name", "Rob"))
	})
	if out != "m svc=bot req.id=7 req.u.name=Rob\n" {
		t.Fatalf("got %q", out)
	}
}

func TestLogHandlerConcurrentLinesStayWhole(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(newLogHandler(&buf, false, false))
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				l.Info("line", "n", fmt.Sprintf("%d-%d", i, j))
			}
		}(i)
	}
	wg.Wait()
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	for _, ln := range lines {
		if !regexp.MustCompile(`^line n=\d+-\d+$`).MatchString(ln) {
			t.Fatalf("interleaved line: %q", ln)
		}
	}
	if len(lines) != 1000 {
		t.Fatalf("got %d lines, want 1000", len(lines))
	}
}

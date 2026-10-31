package timers

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robdevops/signalbot/internal/kit/logx"
)

// start runs ts until the test ends, and waits for Run to return.
func start(t *testing.T, ts *Timers) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { ts.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

type record struct {
	mu   sync.Mutex
	runs []string
	dues []time.Time
	ch   chan struct{}
}

func newRecord() *record { return &record{ch: make(chan struct{}, 100)} }

func (r *record) fn(name string) func(context.Context, time.Time) {
	return func(_ context.Context, due time.Time) {
		r.mu.Lock()
		r.runs = append(r.runs, name)
		r.dues = append(r.dues, due)
		r.mu.Unlock()
		r.ch <- struct{}{}
	}
}

func (r *record) wait(t *testing.T, n int) {
	t.Helper()
	for range n {
		select {
		case <-r.ch:
		case <-time.After(5 * time.Second):
			t.Fatal("timer did not run")
		}
	}
}

func TestRunInOrder(t *testing.T) {
	defer logx.SetOutput(&logx.Buffer{}, &logx.Buffer{}, false)()
	ts, r := New(), newRecord()
	start(t, ts)
	now := time.Now()
	ts.At("b", now.Add(60*time.Millisecond), r.fn("b"))
	ts.At("a", now.Add(20*time.Millisecond), r.fn("a"))
	r.wait(t, 2)
	if strings.Join(r.runs, "") != "ab" {
		t.Errorf("ran %v", r.runs)
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if len(ts.queue) != 0 {
		t.Error("a timer ran but was kept")
	}
}

// A late timer still runs, and is told when it was due.
func TestLateTimerGetsDueTime(t *testing.T) {
	var log logx.Buffer
	defer logx.SetOutput(&log, &log, false)()
	ts, r := New(), newRecord()
	due := time.Now().Add(-3 * time.Hour)
	ts.At("late", due, r.fn("late"))
	start(t, ts)
	r.wait(t, 1)
	if !r.dues[0].Equal(due) {
		t.Errorf("due %v, want %v", r.dues[0], due)
	}
	for deadline := time.Now().Add(5 * time.Second); !strings.Contains(log.String(), "timer late ran in"); {
		if time.Now().After(deadline) {
			t.Fatalf("log %q", log.String())
		}
		time.Sleep(time.Millisecond) // the log line follows the run
	}
}

func TestCancel(t *testing.T) {
	defer logx.SetOutput(&logx.Buffer{}, &logx.Buffer{}, false)()
	ts, r := New(), newRecord()
	start(t, ts)
	cancel, err := ts.At("x", time.Now().Add(20*time.Millisecond), r.fn("x"))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	cancel() // twice is harmless
	ts.At("y", time.Now().Add(40*time.Millisecond), r.fn("y"))
	r.wait(t, 1)
	if strings.Join(r.runs, "") != "y" {
		t.Errorf("ran %v", r.runs)
	}
}

// A timer that panics is logged without its value, and the rest still run.
func TestPanicRecovered(t *testing.T) {
	var log logx.Buffer
	defer logx.SetOutput(&log, &log, false)()
	ts, r := New(), newRecord()
	start(t, ts)
	ts.At("bad", time.Now(), func(context.Context, time.Time) { panic("secret") })
	ts.At("good", time.Now().Add(10*time.Millisecond), r.fn("good"))
	r.wait(t, 1)
	if strings.Contains(log.String(), "secret") || !strings.Contains(log.String(), "running timer bad") {
		t.Errorf("log %q", log.String())
	}
}

func TestCap(t *testing.T) {
	ts := New()
	later := time.Now().Add(time.Hour)
	for i := range Max {
		if _, err := ts.At("t", later, func(context.Context, time.Time) {}); err != nil {
			t.Fatalf("timer %d refused: %v", i, err)
		}
	}
	if _, err := ts.At("t", later, func(context.Context, time.Time) {}); err == nil {
		t.Error("a timer over the cap was set")
	}
}

func TestStoppedRefuses(t *testing.T) {
	ts := New()
	stop, err := ts.At("t", time.Now().Add(time.Hour), func(context.Context, time.Time) {})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ts.Run(ctx)
	stop() // a timer dropped when Run stopped cancels without a panic
	if _, err := ts.At("t", time.Now(), func(context.Context, time.Time) {}); err == nil {
		t.Error("a timer was set after Run stopped")
	}
}

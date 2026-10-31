// Package timers runs one-shot timers on one goroutine.
package timers

import (
	"container/heap"
	"context"
	"errors"
	"sync"
	"time"

	"github.com/robdevops/signalbot/internal/kit/logx"
)

// Max caps the timers set at once.
const Max = 10000

// maxSleep is the longest the goroutine sleeps, so that a suspend or a clock
// jump is noticed within it.
const maxSleep = time.Minute

// Timers keeps every timer in a min-heap, run by Run.
type Timers struct {
	mu      sync.Mutex
	queue   queue
	stopped bool
	wake    chan struct{}
}

type timer struct {
	name  string
	due   time.Time // wall clock only, so comparisons follow clock jumps
	fn    func(context.Context, time.Time)
	index int // in the heap; -1 once run or cancelled
}

// New makes an empty set of timers; Run runs them.
func New() *Timers { return &Timers{wake: make(chan struct{}, 1)} }

// At sets fn to run once at due, however late, given the time it was due.
// fn runs on the timers' goroutine, so it must do only cheap work. At fails
// at the cap or once Run has stopped.
func (t *Timers) At(name string, due time.Time, fn func(ctx context.Context, due time.Time)) (cancel func(), err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	switch {
	case t.stopped:
		return nil, errors.New("timers are stopped")
	case len(t.queue) >= Max:
		return nil, errors.New("too many timers")
	}
	tm := &timer{name: name, due: due.Round(0), fn: fn}
	heap.Push(&t.queue, tm)
	if tm.index == 0 {
		select {
		case t.wake <- struct{}{}:
		default:
		}
	}
	return func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if tm.index >= 0 {
			heap.Remove(&t.queue, tm.index)
		}
	}, nil
}

// Run runs each timer when it is due, until ctx ends. Then the timers left
// are dropped and At fails.
func (t *Timers) Run(ctx context.Context) {
	for {
		t.mu.Lock()
		if ctx.Err() != nil {
			for _, tm := range t.queue {
				tm.index = -1 // so that its cancel does nothing
			}
			t.stopped, t.queue = true, nil
			t.mu.Unlock()
			return
		}
		wait := maxSleep
		if len(t.queue) > 0 {
			if wait = min(wait, time.Until(t.queue[0].due)); wait <= 0 {
				tm := heap.Pop(&t.queue).(*timer)
				t.mu.Unlock()
				run(ctx, tm)
				continue
			}
		}
		t.mu.Unlock()
		sleep := time.NewTimer(wait)
		select {
		case <-ctx.Done():
		case <-t.wake:
		case <-sleep.C:
		}
		sleep.Stop()
	}
}

func run(ctx context.Context, tm *timer) {
	start := time.Now()
	defer func() {
		logx.Debugf("timer %s ran in %.3fs", tm.name, time.Since(start).Seconds())
	}()
	defer logx.Recover("running timer " + tm.name)
	tm.fn(ctx, tm.due)
}

// queue is a min-heap of timers by due time.
type queue []*timer

func (q queue) Len() int           { return len(q) }
func (q queue) Less(i, j int) bool { return q[i].due.Before(q[j].due) }
func (q queue) Swap(i, j int) {
	q[i], q[j] = q[j], q[i]
	q[i].index, q[j].index = i, j
}
func (q *queue) Push(x any) {
	tm := x.(*timer)
	tm.index = len(*q)
	*q = append(*q, tm)
}
func (q *queue) Pop() any {
	old := *q
	tm := old[len(old)-1]
	old[len(old)-1] = nil
	tm.index = -1
	*q = old[:len(old)-1]
	return tm
}

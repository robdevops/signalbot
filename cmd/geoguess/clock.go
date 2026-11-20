package main

import "time"

// clock is the time and timers the game runs on: the real ones, or a test's.
type clock interface {
	Now() time.Time
	// AfterFunc calls f on its own goroutine once d has passed.
	AfterFunc(d time.Duration, f func()) stopper
}

// stopper is a timer that can be stopped before it fires.
type stopper interface{ Stop() bool }

// realClock is the system's clock.
type realClock struct{}

func (realClock) Now() time.Time                              { return time.Now() }
func (realClock) AfterFunc(d time.Duration, f func()) stopper { return time.AfterFunc(d, f) }

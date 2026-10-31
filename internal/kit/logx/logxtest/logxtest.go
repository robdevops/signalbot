// Package logxtest captures the log in tests.
package logxtest

import (
	"bytes"
	"sync"
	"testing"

	"github.com/robdevops/signalbot/internal/kit/logx"
)

// Buf is a buffer safe to write from several goroutines.
type Buf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *Buf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *Buf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (s *Buf) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Len()
}

// Capture collects stdout and stderr lines, without timestamps, until the
// test ends.
func Capture(t testing.TB) *Buf {
	t.Helper()
	buf := &Buf{}
	t.Cleanup(logx.Redirect(buf, buf))
	return buf
}

// Split collects stdout and stderr lines apart.
func Split(t testing.TB) (out, errOut *Buf) {
	t.Helper()
	out, errOut = &Buf{}, &Buf{}
	t.Cleanup(logx.Redirect(out, errOut))
	return out, errOut
}

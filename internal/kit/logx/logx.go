// Package logx writes the log at journald priorities 2 to 7. Every line is
// written. Under journald each line starts with its "<N>" priority; elsewhere
// (a terminal, Docker) lines are plain. Either way priorities 5 to 7 go to
// stdout and 2 to 4 to stderr, so a log driver that sees only the stream can
// still tell them apart.
package logx

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Priority is a syslog priority, which journald reads from a "<N>" prefix.
type Priority int

const (
	Crit   Priority = 2 // an error that stops the bot
	Error  Priority = 3
	Warn   Priority = 4
	Notice Priority = 5 // startup, connect and reconnect
	Info   Priority = 6
	Debug  Priority = 7
)

var (
	mu      sync.Mutex
	stdout  io.Writer = os.Stdout
	stderr  io.Writer = os.Stderr
	journal bool      // stdout is a journald stream: prefix "<N>"
	stamp   bool      // stdout is a terminal: prefix the time
)

// Init looks at stdout: under journald lines carry their priority, and in a
// terminal the time. journald and Docker add their own timestamps.
func Init() {
	mu.Lock()
	defer mu.Unlock()
	journal = stdoutIsJournal()
	st, err := os.Stdout.Stat()
	stamp = err == nil && st.Mode()&os.ModeCharDevice != 0
}

// SetOutput sends the log to out (priorities 5 to 7) and errw (2 to 4), with
// "<N>" prefixes if journald is set and no timestamps, until restore is called.
// It is for tests.
func SetOutput(out, errw io.Writer, journald bool) (restore func()) {
	mu.Lock()
	defer mu.Unlock()
	o, e, j, s := stdout, stderr, journal, stamp
	stdout, stderr, journal, stamp = out, errw, journald, false
	return func() {
		mu.Lock()
		defer mu.Unlock()
		stdout, stderr, journal, stamp = o, e, j, s
	}
}

var escaper = strings.NewReplacer("\n", `\n`, "\r", `\r`)

// Logf writes one line at priority p. Line breaks are escaped, so no value
// (an error body, a display name) can start a forged line.
func Logf(p Priority, format string, args ...any) {
	line := escaper.Replace(fmt.Sprintf(format, args...))
	mu.Lock()
	defer mu.Unlock()
	w := stdout
	if p <= Warn {
		w = stderr
	}
	var b strings.Builder
	if stamp {
		b.WriteString(time.Now().Format("2006/01/02 15:04:05 "))
	}
	if journal {
		fmt.Fprintf(&b, "<%d>", p)
	}
	b.WriteString(line)
	b.WriteByte('\n')
	io.WriteString(w, b.String())
}

func Critf(format string, args ...any)   { Logf(Crit, format, args...) }
func Errorf(format string, args ...any)  { Logf(Error, format, args...) }
func Warnf(format string, args ...any)   { Logf(Warn, format, args...) }
func Noticef(format string, args ...any) { Logf(Notice, format, args...) }
func Infof(format string, args ...any)   { Logf(Info, format, args...) }
func Debugf(format string, args ...any)  { Logf(Debug, format, args...) }

// Recover is deferred where message text is handled. A panic would otherwise
// print its value and a stack trace, and either can hold message text. This
// logs that something failed, never what, and the bot carries on.
func Recover(what string) {
	if recover() != nil {
		Errorf("internal error while %s; dropped", what)
	}
}

// Buffer collects log lines and may be read while they are written. It is
// for tests: SetOutput(buf, buf, false).
type Buffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *Buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// Package logx writes the bot's log: levels, journald priorities under
// systemd, errors and warnings on stderr elsewhere, and newlines escaped.
package logx

import (
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync/atomic"
)

// Level is a syslog priority, which is what journald reads from a "<N>"
// prefix on a line.
type Level int32

const (
	LevelError Level = 3
	LevelWarn  Level = 4
	LevelInfo  Level = 6
	LevelDebug Level = 7
)

var (
	maxLevel  atomic.Int32 // lines above this level are dropped; see SetLevel
	toJournal atomic.Bool  // stdout is a journald stream: mark lines "<N>"
)

func init() { maxLevel.Store(int32(LevelInfo)) }

// errLog takes errors and warnings when journald is not reading the "<N>"
// prefix. They go to stderr, which Docker's journald driver records at
// priority 3 (err) and stdout at 6 (info), so journalctl -p can pick them out.
var errLog = log.New(os.Stderr, "", log.LstdFlags)

// ParseLevel reads LOG_LEVEL: debug, info (the default), warn or error.
func ParseLevel(s string) (Level, bool) {
	switch strings.ToLower(s) {
	case "debug":
		return LevelDebug, true
	case "", "info":
		return LevelInfo, true
	case "warn", "warning":
		return LevelWarn, true
	case "error":
		return LevelError, true
	}
	return LevelInfo, false
}

func SetLevel(l Level) { maxLevel.Store(int32(l)) }

// levelWord marks a line's level where journald is not reading the priority:
// in a terminal or under Docker. Info lines carry no word.
var levelWord = map[Level]string{LevelError: "error: ", LevelWarn: "warning: ", LevelDebug: "debug: "}

func Errorf(format string, args ...any) { logAt(LevelError, format, args...) }
func Warnf(format string, args ...any)  { logAt(LevelWarn, format, args...) }
func Infof(format string, args ...any)  { logAt(LevelInfo, format, args...) }
func Debugf(format string, args ...any) { logAt(LevelDebug, format, args...) }

// logAt writes one line. Newlines are escaped, so no value (an error body, a
// display name) can start a forged line. Under systemd every line goes to
// stdout and starts with its priority, "<3>" for an error, which journald
// turns into the entry's priority and journalctl into its colour. Elsewhere
// a word such as "error: " says the level, and errors and warnings go to
// stderr, the rest to stdout.
func logAt(l Level, format string, args ...any) {
	if int32(l) > maxLevel.Load() {
		return
	}
	line := logEscaper.Replace(fmt.Sprintf(format, args...))
	switch {
	case toJournal.Load():
		log.Printf("<%d>%s", l, line)
	case l <= LevelWarn:
		errLog.Print(levelWord[l] + line)
	default:
		log.Print(levelWord[l] + line)
	}
}

var logEscaper = strings.NewReplacer("\n", `\n`, "\r", `\r`)

// QuietRecover is deferred where message text is handled. A panic would
// otherwise print its value and a stack trace to the log, and either can hold
// message text. This logs that something failed, never what, and the bot
// carries on.
func QuietRecover(what string) {
	if recover() != nil {
		Errorf("internal error while %s; message dropped", what)
	}
}

// Setup sends the log to stdout, without timestamps when journald or Docker
// adds its own, and checks whether stdout is a journald stream.
func Setup() {
	log.SetOutput(os.Stdout)
	if st, err := os.Stdout.Stat(); err != nil || st.Mode()&os.ModeCharDevice == 0 {
		log.SetFlags(0)
	}
	errLog.SetFlags(log.Flags())
	toJournal.Store(stdoutIsJournal())
}

// Redirect sends stdout lines to out and stderr lines to errOut, without
// timestamps, until the returned restore is called. For tests.
func Redirect(out, errOut io.Writer) (restore func()) {
	w, flags := log.Writer(), log.Flags()
	ew, eflags := errLog.Writer(), errLog.Flags()
	log.SetOutput(out)
	log.SetFlags(0)
	errLog.SetOutput(errOut)
	errLog.SetFlags(0)
	return func() {
		log.SetOutput(w)
		log.SetFlags(flags)
		errLog.SetOutput(ew)
		errLog.SetFlags(eflags)
	}
}

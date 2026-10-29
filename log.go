package main

import (
	"fmt"
	"log"
	"strings"
	"sync/atomic"
)

// level is a syslog priority, which is what journald reads from a "<N>"
// prefix on a line.
type level int32

const (
	levelError level = 3
	levelWarn  level = 4
	levelInfo  level = 6
	levelDebug level = 7
)

var (
	maxLevel  atomic.Int32 // lines above this level are dropped; see setLogLevel
	toJournal atomic.Bool  // stdout is a journald stream: mark lines "<N>"
)

func init() { maxLevel.Store(int32(levelInfo)) }

// parseLevel reads LOG_LEVEL: debug, info (the default), warn or error.
func parseLevel(s string) (level, bool) {
	switch strings.ToLower(s) {
	case "debug":
		return levelDebug, true
	case "", "info":
		return levelInfo, true
	case "warn", "warning":
		return levelWarn, true
	case "error":
		return levelError, true
	}
	return levelInfo, false
}

func setLogLevel(l level) { maxLevel.Store(int32(l)) }

// levelWord marks a line's level where journald is not reading the priority:
// in a terminal or under Docker. Info lines carry no word.
var levelWord = map[level]string{levelError: "error: ", levelWarn: "warning: ", levelDebug: "debug: "}

func errorf(format string, args ...any) { logAt(levelError, format, args...) }
func warnf(format string, args ...any)  { logAt(levelWarn, format, args...) }
func infof(format string, args ...any)  { logAt(levelInfo, format, args...) }
func debugf(format string, args ...any) { logAt(levelDebug, format, args...) }

// logAt writes one line through the standard logger, which main points at
// stdout. Newlines are escaped, so no value (an error body, a display name)
// can start a forged line. Under systemd the line starts with its priority,
// "<3>" for an error, which journald turns into the entry's priority and
// journalctl into its colour; elsewhere a word such as "error: " says it.
func logAt(l level, format string, args ...any) {
	if int32(l) > maxLevel.Load() {
		return
	}
	line := logEscaper.Replace(fmt.Sprintf(format, args...))
	if toJournal.Load() {
		line = fmt.Sprintf("<%d>%s", l, line)
	} else {
		line = levelWord[l] + line
	}
	log.Print(line)
}

var logEscaper = strings.NewReplacer("\n", `\n`, "\r", `\r`)

// quietRecover is deferred where message text is handled. A panic would
// otherwise print its value and a stack trace to the log, and either can hold
// message text. This logs that something failed, never what, and the bot
// carries on.
func quietRecover(what string) {
	if recover() != nil {
		errorf("internal error while %s; message dropped", what)
	}
}

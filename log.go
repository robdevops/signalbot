package main

import (
	"fmt"
	"log"
	"strings"
)

var logEscaper = strings.NewReplacer("\n", `\n`, "\r", `\r`)

// quietRecover is deferred where message text is handled. A panic would
// otherwise print its value and a stack trace to the log, and either can hold
// message text. This logs that something failed, never what, and the bot
// carries on.
func quietRecover(what string) {
	if recover() != nil {
		logf("internal error while %s; message dropped", what)
	}
}

// logf writes one line through the standard logger, which main points at
// stdout for journald or Docker. Newlines are escaped, so no value (an error
// body, a display name) can start a forged line.
func logf(format string, args ...any) {
	log.Print(logEscaper.Replace(fmt.Sprintf(format, args...)))
}

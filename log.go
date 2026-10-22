package main

import (
	"fmt"
	"log"
	"strings"
)

var logEscaper = strings.NewReplacer("\n", `\n`, "\r", `\r`)

// logf writes one line through the standard logger, which main points at
// stdout for journald or Docker. Newlines are escaped, so no value (an error
// body, a display name) can start a forged line.
func logf(format string, args ...any) {
	log.Print(logEscaper.Replace(fmt.Sprintf(format, args...)))
}

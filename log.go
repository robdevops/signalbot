package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	logOut        io.Writer = os.Stdout
	logTimestamps bool      // set when interactive; journald and Docker add their own
	logMu         sync.Mutex
)

// logf writes one line to stdout, where journald or Docker captures it.
// Newlines are escaped, so no value (an error body, a display name) can
// start a forged line.
func logf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	line = strings.NewReplacer("\n", `\n`, "\r", `\r`).Replace(line)
	if logTimestamps {
		line = time.Now().Format("2006/01/02 15:04:05 ") + line
	}
	logMu.Lock()
	defer logMu.Unlock()
	io.WriteString(logOut, line+"\n")
}

// isTTY reports whether f is a terminal, i.e. we are running interactively.
func isTTY(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

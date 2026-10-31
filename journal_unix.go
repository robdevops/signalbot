//go:build unix

package main

import (
	"fmt"
	"os"
	"syscall"
)

// stdoutIsJournal reports whether stdout is a stream into journald. systemd
// sets JOURNAL_STREAM to that stream's device and inode; comparing them with
// stdout's own guards against the variable being inherited by a process whose
// output goes elsewhere.
func stdoutIsJournal() bool {
	want := os.Getenv("JOURNAL_STREAM")
	if want == "" {
		return false
	}
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && fmt.Sprintf("%d:%d", st.Dev, st.Ino) == want
}

//go:build unix

package main

import (
	"fmt"
	"os"
	"syscall"
)

// stdoutIsJournal reports whether stdout is connected to journald. systemd
// sets JOURNAL_STREAM to that stream's "device:inode"; comparing them, not
// just checking the variable is set, avoids a false positive in a process
// that merely inherited it, such as a shell in a desktop terminal.
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
	return ok && want == fmt.Sprintf("%d:%d", st.Dev, st.Ino)
}

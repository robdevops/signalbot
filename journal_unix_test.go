//go:build unix

package main

import (
	"fmt"
	"os"
	"syscall"
	"testing"
)

func TestStdoutIsJournal(t *testing.T) {
	t.Setenv("JOURNAL_STREAM", "")
	if stdoutIsJournal() {
		t.Fatal("unset JOURNAL_STREAM must not count as the journal")
	}
	t.Setenv("JOURNAL_STREAM", "1:2")
	if stdoutIsJournal() {
		t.Fatal("JOURNAL_STREAM naming another stream must not count (inherited variable)")
	}
	fi, err := os.Stdout.Stat()
	if err != nil {
		t.Skip(err)
	}
	st := fi.Sys().(*syscall.Stat_t)
	t.Setenv("JOURNAL_STREAM", fmt.Sprintf("%d:%d", st.Dev, st.Ino))
	if !stdoutIsJournal() {
		t.Fatal("JOURNAL_STREAM matching stdout's device:inode is the journal")
	}
}

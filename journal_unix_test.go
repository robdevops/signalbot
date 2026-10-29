//go:build unix

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Only a stdout that is the very stream named in JOURNAL_STREAM counts.
func TestStdoutIsJournal(t *testing.T) {
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fi, _ := f.Stat()
	st := fi.Sys().(*syscall.Stat_t)
	old := os.Stdout
	os.Stdout = f
	t.Cleanup(func() { os.Stdout = old })

	t.Setenv("JOURNAL_STREAM", "")
	if stdoutIsJournal() {
		t.Error("journal detected with no JOURNAL_STREAM")
	}
	t.Setenv("JOURNAL_STREAM", fmt.Sprintf("%d:%d", st.Dev, st.Ino))
	if !stdoutIsJournal() {
		t.Error("journal not detected when stdout is the named stream")
	}
	t.Setenv("JOURNAL_STREAM", fmt.Sprintf("%d:%d", st.Dev, st.Ino+1))
	if stdoutIsJournal() {
		t.Error("an inherited JOURNAL_STREAM for another stream was trusted")
	}
}

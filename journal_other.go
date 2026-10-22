//go:build !unix

package main

// stdoutIsJournal: journald only exists on unix systems.
func stdoutIsJournal() bool { return false }

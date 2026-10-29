//go:build !unix

package main

// stdoutIsJournal is false where there is no journald.
func stdoutIsJournal() bool { return false }

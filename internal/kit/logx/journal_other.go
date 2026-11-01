//go:build !unix

package logx

// stdoutIsJournal is false where there is no journald.
func stdoutIsJournal() bool { return false }

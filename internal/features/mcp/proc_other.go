//go:build !unix

package mcp

import (
	"os"
	"os/exec"
	"syscall"
)

// setGroup does nothing where there are no process groups.
func setGroup(*exec.Cmd) {}

// signalGroup kills p, whatever sig is, where there are no process groups.
func signalGroup(p *os.Process, _ syscall.Signal) { p.Kill() }

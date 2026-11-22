//go:build unix

package mcp

import (
	"os"
	"os/exec"
	"syscall"
)

// setGroup makes cmd's process lead a process group of its own.
func setGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

// signalGroup sends sig to p's process group.
func signalGroup(p *os.Process, sig syscall.Signal) { syscall.Kill(-p.Pid, sig) }

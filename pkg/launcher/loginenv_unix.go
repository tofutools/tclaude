//go:build linux || darwin

package launcher

import (
	"os/exec"
	"syscall"
)

// setNewSession runs cmd in a session of its own, so an interactive shell
// finds no controlling terminal to read from, and a timeout kills the whole
// session (a profile's background jobs too), not only the shell.
func setNewSession(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// killSession kills what is left of cmd's session once cmd has exited.
func killSession(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

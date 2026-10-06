//go:build linux || darwin

package agentd

import (
	"os/exec"
	"syscall"
)

// configureOpenCodeProcessGroup keeps the server and HTTP bridge workload in
// a launch-owned group, including on macOS where subtree enumeration is absent.
func configureOpenCodeProcessGroup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

func killOpenCodeProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil || cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid {
		return
	}
	// Never signal the daemon's group or a group whose leader already exited.
	if group, err := syscall.Getpgid(cmd.Process.Pid); err == nil && group == cmd.Process.Pid {
		_ = syscall.Kill(-group, syscall.SIGKILL)
	}
}

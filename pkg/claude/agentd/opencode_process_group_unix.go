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
	// Setpgid assigned this launch its own group with the recorded leader PID.
	// Descendants retain that group after the leader exits, so Getpgid(leader)
	// cannot be used here. ESRCH simply means the group is already empty.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

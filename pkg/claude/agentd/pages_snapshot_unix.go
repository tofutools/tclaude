//go:build unix

package agentd

import (
	"os/exec"
	"syscall"
)

// chromeProcessGroup starts Chrome in a process group of its own, so killing
// it takes its renderer and helper processes along.
func chromeProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killChrome(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	_ = cmd.Process.Kill()
}

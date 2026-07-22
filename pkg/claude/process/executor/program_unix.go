//go:build linux || darwin

package executor

import (
	"errors"
	osexec "os/exec"
	"syscall"
)

var killProgramProcessGroup = func(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}

func configureProgramCommand(command *osexec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return ignoreNoProcess(killProgramProcessGroup(command.Process.Pid))
	}
	command.WaitDelay = programWaitDelay
}

// Kill the private process group after Wait so ordinary descendants cannot be
// left behind when their group leader exits. A separately authorized sandbox
// is still required to contain a hostile process that deliberately escapes its
// group with setpgid or setsid.
func cleanupProgramCommand(command *osexec.Cmd) error {
	if command.Process != nil {
		return ignoreNoProcess(killProgramProcessGroup(command.Process.Pid))
	}
	return nil
}

func ignoreNoProcess(err error) error {
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

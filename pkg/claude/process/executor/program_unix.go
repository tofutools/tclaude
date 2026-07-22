//go:build linux || darwin

package executor

import (
	"errors"
	osexec "os/exec"
	"syscall"
)

func configureProgramCommand(command *osexec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return ignoreNoProcess(syscall.Kill(-command.Process.Pid, syscall.SIGKILL))
	}
	command.WaitDelay = programWaitDelay
}

// Always kill the private process group after Wait. This removes descendants
// deliberately left behind by a program even when the group leader exited.
func cleanupProgramCommand(command *osexec.Cmd) {
	if command.Process != nil {
		_ = ignoreNoProcess(syscall.Kill(-command.Process.Pid, syscall.SIGKILL))
	}
}

func ignoreNoProcess(err error) error {
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

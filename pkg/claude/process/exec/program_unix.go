//go:build linux || darwin

package processexec

import (
	"errors"
	osexec "os/exec"
	"syscall"
	"time"
)

func configureProgramCommand(command *osexec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	command.WaitDelay = time.Second
}

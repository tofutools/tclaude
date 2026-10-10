//go:build !windows

package nodeinfo

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func configureProbeProcess(cmd *exec.Cmd) {
	// Harness launchers can run a native child even for --version (Copilot
	// extracts its executable first). Cancellation must stop those writers
	// before the caller tears down its environment, not just kill the launcher.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}

//go:build linux || darwin

package claude

import (
	"os/exec"
	"syscall"
)

// Cancel the whole process group so a timed-out shell command cannot leave
// children running after the foreground parent exits.
func configureRunProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}

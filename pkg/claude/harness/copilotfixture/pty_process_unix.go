//go:build linux || darwin

package copilotfixture

import (
	"errors"
	"os/exec"
	"syscall"
)

func configurePTYCommand(cmd *exec.Cmd) {
	// pty.StartWithSize starts the child in a new session, which also makes it
	// the leader of a private process group. Override CommandContext's default
	// parent-only kill so an early verdict stops Copilot's helpers too.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return ignoreMissingPTYProcess(syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL))
	}
}

// Kill the private process group after Wait as well. Interactive runs normally
// end through Cancel, but a CLI that exits on its own can still leave a helper
// behind. RunPTY must not return while that helper can still mutate a t.TempDir.
func cleanupPTYCommand(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return ignoreMissingPTYProcess(syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL))
}

func ignoreMissingPTYProcess(err error) error {
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

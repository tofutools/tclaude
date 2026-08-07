//go:build linux || darwin

package copilotfixture

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

const ptyProcessExitGrace = 2 * time.Second

type signalPTYProcessGroup func(pid int, signal syscall.Signal) error

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
	return cleanupPTYProcessGroup(cmd.Process.Pid, ptyProcessExitGrace, syscall.Kill)
}

func cleanupPTYProcessGroup(
	pid int, grace time.Duration, signal signalPTYProcessGroup,
) error {
	if err := signal(-pid, syscall.SIGKILL); err != nil {
		return ignoreMissingPTYProcess(err)
	}

	deadline := time.Now().Add(grace)
	for {
		err := signal(-pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if err != nil {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("process group %d survived SIGKILL for %s",
				pid, grace)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func ignoreMissingPTYProcess(err error) error {
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

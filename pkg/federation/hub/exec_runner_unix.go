//go:build linux || darwin

package hub

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/tofutools/tclaude/pkg/noderun"
)

func executeHubScript(parent context.Context, path, _ string, seconds int64, stdout, stderr io.Writer) noderun.Result {
	if os.Geteuid() == 0 {
		return noderun.Result{ExitCode: 125, Error: "hub scripts cannot run as root"}
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(seconds)*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, "/bin/sh", path)
	command.Stdout, command.Stderr = stdout, stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.WaitDelay = 2 * time.Second
	command.Cancel = func() error {
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	err := command.Run()
	// Shell children must not outlive a completed, canceled or timed-out job.
	if command.Process != nil {
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	result := noderun.Result{ExitCode: 0}
	if err != nil {
		result.ExitCode = 125
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			result.ExitCode = exit.ExitCode()
			if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				result.ExitCode = 128 + int(status.Signal())
			}
		} else {
			result.Error = "could not execute hub script"
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.Error = "script timed out"
		result.TimedOut = true
	}
	return result
}

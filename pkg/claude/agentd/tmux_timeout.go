package agentd

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// tmuxCommandTimeout bounds every tmux subprocess on the nudge path. A tmux
// client can occasionally connect but never return; without a deadline that
// parks the per-target nudge worker forever and every later enqueue merely
// coalesces behind it. Five seconds is generous for a local tmux server while
// still turning a wedged client into a visible, retryable failure.
var tmuxCommandTimeout = 5 * time.Second

var errTmuxCommandTimeout = errors.New("tmux command timeout")

func runTmuxCommand(args ...string) error {
	return runCommandWithTimeout(clcommon.TmuxCommand(args...), tmuxCommandTimeout)
}

// runCommandWithTimeout is deliberately expressed over *exec.Cmd rather than
// exec.CommandContext: the injected clcommon.Tmux boundary constructs the
// command (real tmux in production, TmuxSim in flows). Killing the process on
// timeout preserves that boundary and, critically, lets cmd.Wait reap it.
func runCommandWithTimeout(cmd *exec.Cmd, timeout time.Duration) error {
	return runCommandWithTimeoutTimer(cmd, timeout, func(timeout time.Duration) (<-chan time.Time, func()) {
		timer := time.NewTimer(timeout)
		return timer.C, func() { _ = timer.Stop() }
	})
}

// runCommandWithTimeoutTimer exposes only timer construction to tests. Keeping
// command startup, process killing, and reaping on the production path lets a
// test trigger the timeout after its helper process has proved it is running.
func runCommandWithTimeoutTimer(
	cmd *exec.Cmd,
	timeout time.Duration,
	startTimer func(time.Duration) (<-chan time.Time, func()),
) error {
	var stderr bytes.Buffer
	if cmd.Stderr == nil {
		cmd.Stderr = &stderr
	}
	if timeout <= 0 {
		return tmuxCommandError(cmd.Run(), stderr.String())
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timedOut, stopTimer := startTimer(timeout)
	defer stopTimer()
	select {
	case err := <-done:
		return tmuxCommandError(err, stderr.String())
	case <-timedOut:
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done // reap the child before returning and releasing the queue latch
		if detail := strings.TrimSpace(stderr.String()); detail != "" {
			return fmt.Errorf("%w after %s: %s", errTmuxCommandTimeout, timeout, detail)
		}
		return fmt.Errorf("%w after %s", errTmuxCommandTimeout, timeout)
	}
}

func tmuxCommandError(err error, stderr string) error {
	if err == nil {
		return nil
	}
	if detail := strings.TrimSpace(stderr); detail != "" {
		return fmt.Errorf("%w: %s", err, detail)
	}
	return err
}

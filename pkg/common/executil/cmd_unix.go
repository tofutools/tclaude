//go:build !windows

package executil

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

func setup(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pgid = 0
}

func (c *Cmd) watch() {
	select {
	case <-c.done:
		return
	case <-c.ctx.Done():
	}
	if c.Process == nil {
		return
	}
	pgid := c.Process.Pid
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	timer := time.NewTimer(c.gracePeriod)
	defer timer.Stop()
	select {
	case <-c.done:
		return
	case <-timer.C:
	}
	// One killpg can miss a member that is mid-fork as it is delivered, and
	// that orphan keeps holding whatever pipes it inherited. Repeat the kill
	// until the group is gone (ESRCH), the command has been waited for, or a
	// bounded number of retries — an unreaped leader keeps the group alive.
	ticker := time.NewTicker(groupKillRetry)
	defer ticker.Stop()
	for range groupKillRetries {
		if err := syscall.Kill(-pgid, syscall.SIGKILL); errors.Is(err, syscall.ESRCH) {
			return
		}
		select {
		case <-c.done:
			return
		case <-ticker.C:
		}
	}
}

// groupKillRetry spaces the repeated SIGKILLs of a process group that
// outlived its grace period; groupKillRetries bounds them (about 2s).
const (
	groupKillRetry   = 50 * time.Millisecond
	groupKillRetries = 40
)

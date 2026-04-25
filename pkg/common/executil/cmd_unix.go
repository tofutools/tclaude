//go:build !windows

package executil

import (
	"os/exec"
	"syscall"
	"time"
)

func setup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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
	case <-timer.C:
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
}

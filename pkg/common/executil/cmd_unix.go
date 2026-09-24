//go:build !windows

package executil

import (
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
	case <-timer.C:
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
	}
}

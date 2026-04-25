//go:build windows

package executil

import (
	"os/exec"
	"strconv"
	"time"
)

func setup(_ *exec.Cmd) {}

func (c *Cmd) watch() {
	select {
	case <-c.done:
		return
	case <-c.ctx.Done():
	}
	if c.Process == nil {
		return
	}
	pid := c.Process.Pid
	// taskkill /T kills the process tree; /F forces immediate termination
	_ = exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(pid)).Run()
	_ = c.Process.Kill()
	timer := time.NewTimer(c.gracePeriod)
	defer timer.Stop()
	select {
	case <-c.done:
	case <-timer.C:
	}
}

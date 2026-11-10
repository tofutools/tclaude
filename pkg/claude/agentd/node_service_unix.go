//go:build linux || darwin

package agentd

import (
	"context"
	"os/exec"
	"syscall"
)

// detachedLauncherRun runs a supervisor command in its own session, so a
// supervisor stopping our process group does not take the request with it.
func detachedLauncherRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd.Output()
}

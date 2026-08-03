//go:build !linux && !darwin

package agentd

import (
	"os/exec"
	"time"
)

// proxyWaitDelay bounds pipe draining after cancellation. See the unix twin.
const proxyWaitDelay = time.Second

// configureProxyCommand is the portable fallback for platforms tclaude does
// not target (see CLAUDE.md: Linux and macOS, with WSL treated as Linux).
// There is no process-group handling here, so cancellation reaches only the
// direct child; a transport helper could briefly outlive it.
func configureProxyCommand(cmd *exec.Cmd) {
	cmd.WaitDelay = proxyWaitDelay
}

func cleanupProxyCommand(*exec.Cmd) error { return nil }

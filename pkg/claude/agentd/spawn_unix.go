//go:build !windows

package agentd

import (
	"os/exec"
	"syscall"
)

// detachSpawn configures cmd so the spawned wrapper:
//   - sits in its own session (no controlling tty)
//   - is its own process group leader
//
// When our daemon (its parent) exits, the wrapper gets reparented to
// init/PID 1 immediately. The actual CC process started by `tclaude
// session new` lives inside the tmux server and is already independent
// of either us or the wrapper — but giving the wrapper a clean
// detachment posture (no inherited terminal, own session) keeps the
// process tree tidy and avoids any ambient terminal lifecycle quirks
// from leaking into the child.
//
// Unix-only: agentd compiles only on linux/darwin (see peer_*.go).
func detachSpawn(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}
}

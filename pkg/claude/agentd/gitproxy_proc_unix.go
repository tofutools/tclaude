//go:build linux || darwin

package agentd

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

// proxyWaitDelay is how long a proxied subprocess gets to drain its pipes after
// its context is cancelled before Go force-closes them. Short: by this point we
// have already killed the process group.
const proxyWaitDelay = time.Second

// killProxyProcessGroup is a var so a unit test can observe the reap without
// signalling anything real.
var killProxyProcessGroup = func(pid int) error {
	return syscall.Kill(-pid, syscall.SIGKILL)
}

// configureProxyCommand puts a proxied subprocess in its own process group and
// kills the whole group on cancellation.
//
// This matters more for the proxy than for an ordinary subprocess: `git fetch`
// and `git push` spawn transport helpers (git-remote-https, ssh), and those
// children hold the network connection. Killing only the group leader on a
// timeout would leave an ssh holding a credential-authenticated session open
// with nothing watching it.
func configureProxyCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return ignoreProxyNoProcess(killProxyProcessGroup(cmd.Process.Pid))
	}
	cmd.WaitDelay = proxyWaitDelay
}

// cleanupProxyCommand reaps the private process group after Wait, so an
// ordinary descendant cannot be left behind when its leader exits. A process
// that deliberately escapes its group with setsid still needs a real OS
// sandbox to contain — that is not this layer's job.
func cleanupProxyCommand(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return ignoreProxyNoProcess(killProxyProcessGroup(cmd.Process.Pid))
}

func ignoreProxyNoProcess(err error) error {
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

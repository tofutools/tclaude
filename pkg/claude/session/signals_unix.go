//go:build !windows

package session

import (
	"os"
	"syscall"
)

// tmuxSignals returns the signals to forward to tmux.
func tmuxSignals() []os.Signal {
	return []os.Signal{syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGWINCH}
}

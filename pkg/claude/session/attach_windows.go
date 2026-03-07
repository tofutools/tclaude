//go:build windows

package session

import "fmt"

// attachToSession is not supported on Windows native (no tmux).
func attachToSession(tmuxSession string) error {
	return fmt.Errorf("tmux sessions not supported on Windows native")
}

// attachToSessionWithFlags is not supported on Windows native (no tmux).
func attachToSessionWithFlags(tmuxSession string, force bool) error {
	return fmt.Errorf("tmux sessions not supported on Windows native")
}

//go:build !windows

package session

import (
	"fmt"
	"os"
	"os/exec"
)

// attachToSession attaches to a tmux session as a subprocess.
func attachToSession(tmuxSession string) error {
	return attachToSessionWithFlags(tmuxSession, false)
}

// attachToSessionWithFlags attaches to a tmux session with optional force flag.
// If force is true, uses -d to detach other clients.
// Runs tmux as a subprocess so we return when the user detaches.
func attachToSessionWithFlags(tmuxSession string, force bool) error {
	args := []string{"attach-session", "-t", tmuxSession}
	if force {
		args = []string{"attach-session", "-d", "-t", tmuxSession}
	}

	cmd := exec.Command("tmux", args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		// Exit status 1 typically means session ended or detached - not an error
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return nil
		}
		return fmt.Errorf("tmux attach failed: %w", err)
	}
	return nil
}

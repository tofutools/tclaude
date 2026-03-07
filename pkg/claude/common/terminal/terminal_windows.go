//go:build windows

package terminal

import (
	"os/exec"
)

// OpenWithCommand opens a new terminal window running the given command.
func OpenWithCommand(command string) error {
	// Try Windows Terminal first
	if _, err := exec.LookPath("wt.exe"); err == nil {
		return exec.Command("wt.exe", "new-tab", "cmd", "/k", command).Start()
	}
	// Fallback: open new cmd window running the command
	return exec.Command("cmd", "/c", "start", "cmd", "/k", command).Start()
}

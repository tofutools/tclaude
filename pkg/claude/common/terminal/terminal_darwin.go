//go:build darwin

package terminal

import (
	"os/exec"
	"strings"
)

// OpenWithCommand opens a new terminal window running the given command.
func OpenWithCommand(command string) error {
	// AppleScript to open Terminal.app with command
	script := `tell application "Terminal"
	activate
	do script "` + escapeAppleScript(command) + `"
end tell`

	return exec.Command("osascript", "-e", script).Run()
}

// escapeAppleScript escapes a string for use in AppleScript.
func escapeAppleScript(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	return s
}

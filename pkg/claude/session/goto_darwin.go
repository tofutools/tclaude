//go:build darwin

package session

import (
	"fmt"
	"os/exec"
	"strings"
)

// focusTTY focuses the terminal window/tab owning the given TTY.
// Tries iTerm2 first, then Terminal.app.
func focusTTY(tty string) bool {
	// Try iTerm2 directly (skip System Events check for speed)
	script := fmt.Sprintf(`
tell application "iTerm2"
	activate
	repeat with w in windows
		repeat with t in tabs of w
			repeat with s in sessions of t
				if tty of s is "%s" then
					select t
					select w
					return true
				end if
			end repeat
		end repeat
	end repeat
end tell
`, tty)

	cmd := exec.Command("osascript", "-e", script)
	output, err := cmd.Output()
	if err == nil && strings.TrimSpace(string(output)) == "true" {
		return true
	}

	// Fallback: Terminal.app
	script = fmt.Sprintf(`
tell application "Terminal"
	activate
	repeat with w in windows
		repeat with t in tabs of w
			if tty of t is "%s" then
				set selected of t to true
				set index of w to 1
				return true
			end if
		end repeat
	end repeat
end tell
`, tty)

	cmd = exec.Command("osascript", "-e", script)
	output, err = cmd.Output()
	return err == nil && strings.TrimSpace(string(output)) == "true"
}

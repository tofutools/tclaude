//go:build darwin

package terminal

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// OpenWithCommand opens a new terminal window running the given command.
// It prefers iTerm2 when that app is installed, falling back to the
// built-in Terminal.app otherwise. The fallback also covers the case
// where iTerm2 is present but fails to open a window.
func OpenWithCommand(command string) error {
	debug := os.Getenv("TCLAUDE_DEBUG") != ""

	if isAppInstalled("iTerm") {
		if err := runOsascript(iTermScript(command), "iTerm2", debug); err == nil {
			return nil
		} else if debug {
			fmt.Printf("[debug] terminal: iTerm2 launch failed, falling back to Terminal.app: %v\n", err)
		}
	}
	return runOsascript(terminalAppScript(command), "Terminal.app", debug)
}

// iTermScript builds the AppleScript that opens a new iTerm2 window with
// the default profile and types command into it. The default profile
// starts the user's login shell — full PATH/env — and `write text` then
// runs the command inside it. This deliberately avoids
// `create window with default profile command "..."`, which falls into
// the launchd-PATH trap where bare execs can't find homebrew binaries
// like tmux.
func iTermScript(command string) string {
	return `tell application "iTerm2"
	activate
	set newWindow to (create window with default profile)
	tell current session of newWindow
		write text "` + escapeAppleScript(command) + `"
	end tell
end tell`
}

// terminalAppScript builds the AppleScript that opens a new Terminal.app
// window running command via `do script` (keystroke-fed into a fresh shell).
func terminalAppScript(command string) string {
	return `tell application "Terminal"
	activate
	do script "` + escapeAppleScript(command) + `"
end tell`
}

// runOsascript executes an AppleScript via osascript, logging the script
// and its output when TCLAUDE_DEBUG is set.
func runOsascript(script, label string, debug bool) error {
	if debug {
		fmt.Printf("[debug] terminal: %s AppleScript: %s\n", label, strings.TrimSpace(script))
	}
	out, err := exec.Command("osascript", "-e", script).CombinedOutput()
	if debug {
		fmt.Printf("[debug] terminal: %s output=%q err=%v\n", label, strings.TrimSpace(string(out)), err)
	}
	return err
}

// isAppInstalled reports whether the named application bundle can be
// located on disk. `osascript -e 'id of application "Name"'` succeeds
// iff the app exists.
func isAppInstalled(appName string) bool {
	return exec.Command("osascript", "-e",
		`id of application "`+escapeAppleScript(appName)+`"`).Run() == nil
}

// escapeAppleScript escapes a string for safe interpolation inside an
// AppleScript double-quoted literal.
func escapeAppleScript(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	return s
}

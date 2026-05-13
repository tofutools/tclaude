//go:build darwin

package session

import (
	"fmt"
	"os/exec"
	"strings"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
)

// openTerminalAttachingSession spawns a new terminal window that runs
// `tclaude session attach <sessionID>`. Tries iTerm2 first (real `command`
// API on `create window`), then Terminal.app (`do script`). Uses the
// absolute path to the current tclaude binary so PATH need not contain it.
func openTerminalAttachingSession(sessionID string, debug bool) bool {
	if sessionID == "" {
		return false
	}

	cmd := clcommon.DetectAbsoluteCmd("session", "attach", sessionID)
	if debug {
		fmt.Printf("[debug] openTerminalAttachingSession: cmd=%q\n", cmd)
	}

	if isAppInstalled("iTerm") {
		if spawnITermWithCommand(cmd, debug) {
			return true
		}
	}
	return spawnTerminalAppWithCommand(cmd, debug)
}

// spawnITermWithCommand opens a new iTerm2 window with the default profile
// (so the user's login shell starts with their full PATH/env) and then types
// the command into it via `write text`. Using the default profile rather
// than `create window with default profile command "..."` avoids the
// launchd-PATH trap where bare execs can't find homebrew binaries like tmux.
func spawnITermWithCommand(command string, debug bool) bool {
	script := fmt.Sprintf(`
tell application "iTerm2"
	activate
	set newWindow to (create window with default profile)
	tell current session of newWindow
		write text "%s"
	end tell
end tell
`, escapeAppleScriptString(command))

	if debug {
		fmt.Printf("[debug] iTerm2 spawn AppleScript: %s\n", strings.TrimSpace(script))
	}

	c := exec.Command("osascript", "-e", script)
	out, err := c.CombinedOutput()
	if debug {
		fmt.Printf("[debug] iTerm2 spawn output=%q err=%v\n", strings.TrimSpace(string(out)), err)
	}
	return err == nil
}

// spawnTerminalAppWithCommand opens a new Terminal.app window running the
// given command via `do script` (keystroke-fed into a fresh shell).
func spawnTerminalAppWithCommand(command string, debug bool) bool {
	script := fmt.Sprintf(`tell application "Terminal"
	activate
	do script "%s"
end tell`, escapeAppleScriptString(command))

	if debug {
		fmt.Printf("[debug] Terminal.app spawn AppleScript: %s\n", strings.TrimSpace(script))
	}

	c := exec.Command("osascript", "-e", script)
	out, err := c.CombinedOutput()
	if debug {
		fmt.Printf("[debug] Terminal.app spawn output=%q err=%v\n", strings.TrimSpace(string(out)), err)
	}
	return err == nil
}

// isAppInstalled returns true if the named application bundle exists on disk.
func isAppInstalled(appName string) bool {
	// `osascript -e 'id of application "Name"'` succeeds iff the app can be located.
	script := fmt.Sprintf(`id of application "%s"`, escapeAppleScriptString(appName))
	c := exec.Command("osascript", "-e", script)
	return c.Run() == nil
}

// escapeAppleScriptString escapes a string for safe interpolation inside
// AppleScript double-quoted literals.
func escapeAppleScriptString(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	return s
}

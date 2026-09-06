package notify

import (
	"fmt"
	"os/exec"
	"path/filepath"

	"github.com/tofutools/tclaude/pkg/claude/common"
)

// platformSend sends a notification using macOS-specific methods.
func platformSend(sessionID, title, body string) error {
	return sendDarwinClickable(sessionID, title, body)
}

// sendDarwinClickable sends a notification with click-to-focus on macOS.
func sendDarwinClickable(sessionID, title, body string) error {
	// Check for terminal-notifier (supports -execute)
	if _, err := exec.LookPath("terminal-notifier"); err == nil {
		// Use absolute path because terminal-notifier -execute runs in a
		// minimal shell environment where tclaude may not be on PATH.
		// DetectAbsoluteCmd shell-quotes the path so spaces don't break -execute.
		clCmd := common.DetectAbsoluteCmd()

		// Get full path to tmux (needed by focus command)
		tmuxPath, err := exec.LookPath("tmux")
		if err != nil {
			tmuxPath = "" // will use PATH
		}

		// Build command - terminal-notifier runs with minimal PATH
		var focusCmd string
		if tmuxPath != "" {
			// Add tmux's directory to PATH
			tmuxDir := filepath.Dir(tmuxPath)
			focusCmd = fmt.Sprintf("PATH=%s:$PATH %s session focus %s",
				tmuxDir, clCmd, sessionID)
		} else {
			focusCmd = fmt.Sprintf("%s session focus %s", clCmd, sessionID)
		}

		return exec.Command("terminal-notifier",
			"-title", title,
			"-message", body,
			"-execute", focusCmd,
			"-sound", "default",
		).Run()
	}

	// Fallback to osascript notification (no click action)
	return darwinNotifyCommand(title, body).Run()
}

// darwinNotifyScript is the osascript fallback, and is a compile-time
// CONSTANT: the title and body arrive through argv and never become
// AppleScript source. Interpolating them into the source instead — even
// with the quotes escaped — is a code-execution sink, because escaping the
// quote without first escaping the backslash lets a trailing `\` in the
// value consume the added escape, close the string, and run whatever
// AppleScript (`do shell script` included) follows it. The values reaching
// here are agent-authored (a notify-human body, a present-pr summary), so
// that is reachable input, not a theoretical one.
//
// argv is 1-based and starts with a fixed sentinel: osascript parses its
// own options up to the first non-option argument, so a body beginning with
// "-e" must not be the first thing after the script. The sentinel is that
// first thing, and the untrusted values sit safely behind it.
const darwinNotifyScript = `on run argv
	display notification (item 2 of argv) with title (item 3 of argv)
end run`

// darwinNotifySentinel is the fixed argv[1] that terminates osascript's own
// option parsing. Its value is never read by the script.
const darwinNotifySentinel = "tclaude-notify"

// darwinNotifyCommand builds the osascript fallback invocation. Split out
// from sendDarwinClickable so a test can assert the argv shape — that the
// script is the constant and the untrusted values are separate arguments —
// without running osascript.
func darwinNotifyCommand(title, body string) *exec.Cmd {
	return exec.Command("osascript", "-e", darwinNotifyScript, darwinNotifySentinel, body, title)
}

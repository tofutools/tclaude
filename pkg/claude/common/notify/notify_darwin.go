package notify

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

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
		clCmd := strings.Join(common.DetectAbsoluteArgs(), " ")

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
	script := fmt.Sprintf(`display notification "%s" with title "%s"`,
		strings.ReplaceAll(body, "\"", "\\\""),
		strings.ReplaceAll(title, "\"", "\\\""))
	return exec.Command("osascript", "-e", script).Run()
}

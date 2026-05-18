//go:build darwin

package session

import (
	"fmt"

	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/terminal"
)

// openTerminalAttachingSession spawns a new terminal window that runs
// `tclaude session attach <sessionID>`. Terminal selection — iTerm2 when
// installed, else Terminal.app — is handled by terminal.OpenWithCommand.
// Uses the absolute path to the current tclaude binary so PATH need not
// contain it.
//
// The `exec ` prefix replaces the wrapping interactive shell with
// tclaude, so when a later "hide" detaches the tmux client and tclaude
// exits, no shell is left holding the tab open. Same rationale as
// agentd's openAttachCmd — without it the iTerm2 / Terminal.app
// AppleScript drivers (which type the command into a default-profile
// interactive shell) would return to a prompt instead of closing the
// tab. Unconditional because this file is darwin-only.
func openTerminalAttachingSession(sessionID string, debug bool) bool {
	if sessionID == "" {
		return false
	}

	cmd := "exec " + clcommon.DetectAbsoluteCmd("session", "attach", sessionID)
	if debug {
		fmt.Printf("[debug] openTerminalAttachingSession: cmd=%q\n", cmd)
	}

	if err := terminal.OpenWithCommand(cmd); err != nil {
		if debug {
			fmt.Printf("[debug] openTerminalAttachingSession: open failed: %v\n", err)
		}
		return false
	}
	return true
}

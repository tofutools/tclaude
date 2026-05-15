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
func openTerminalAttachingSession(sessionID string, debug bool) bool {
	if sessionID == "" {
		return false
	}

	cmd := clcommon.DetectAbsoluteCmd("session", "attach", sessionID)
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

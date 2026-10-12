package session

import (
	"fmt"
	"slices"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// RequireConfinementForApproval refuses a fresh launch whose approval posture
// runs commands unattended on a harness with no sandbox of its own, unless
// tclaude's sandbox confines it or the human explicitly allowed an unenforced
// launch. A continuation (resume, reincarnation, no-copy clone) carries a
// posture that was already authorized when the agent was first launched.
func RequireConfinementForApproval(h *harness.Harness, approvalPolicy string, confined, allowUnenforced, continuation bool) error {
	if h == nil {
		return nil
	}
	if approvalPolicy == "" && h.Approval != nil {
		// No explicit posture launches the harness default.
		approvalPolicy = h.Approval.DefaultPolicy()
	}
	if !slices.Contains(h.ConfinedApprovals, approvalPolicy) {
		return nil
	}
	if confined || allowUnenforced || continuation {
		return nil
	}
	return fmt.Errorf("%s %q runs every tool, including shell commands, with no prompt, "+
		"and %s has no sandbox of its own, so it launches only inside tclaude's sandbox; "+
		"this launch is not sandboxed. Ask the human to spawn it from the dashboard with "+
		"\"Allow launch with unenforced rules\", or pass --ask-for-approval for a narrower posture",
		h.DisplayName, approvalPolicy, h.DisplayName)
}

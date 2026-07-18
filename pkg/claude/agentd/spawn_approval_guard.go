package agentd

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// spawnApprovalLineageFailure prevents an agent from minting a child with
// broader automatic command-acceptance capability than its own launch
// posture. Humans have no caller conv-id and remain the trust root.
func spawnApprovalLineageFailure(parentConvID, childHarness, childPolicy string, childAutoReview bool) *spawnFailure {
	if parentConvID == "" {
		return nil
	}
	parent, err := db.FindSessionByConvID(parentConvID)
	if err != nil {
		return &spawnFailure{http.StatusInternalServerError, "io", "spawn approval guard: " + err.Error()}
	}
	if parent == nil {
		return &spawnFailure{http.StatusForbidden, "approval_restricted",
			fmt.Sprintf("agent %s has no recorded launch approval posture; relaunch it before spawning children", short8(parentConvID))}
	}
	parentHarness := harnessOrDefault(parent.Harness)
	parentPolicy := strings.TrimSpace(parent.ApprovalPolicy)
	parentAutoReview := parent.ApprovalAutoReview
	if parentHarness == harness.CodexName && parentPolicy == "" {
		legacyPolicy, proven, inferErr := db.LegacyCodexApprovalForConv(parentConvID)
		if inferErr != nil {
			return &spawnFailure{http.StatusInternalServerError, "io", "spawn approval guard: " + inferErr.Error()}
		}
		if proven {
			parentPolicy = legacyPolicy
			// Every reconstructable legacy never path either launched without a
			// reviewer or had an idle reviewer (never emits no approval request).
			parentAutoReview = false
		}
	}
	childHarness = harnessOrDefault(childHarness)
	childPolicy = strings.TrimSpace(childPolicy)
	if parentPolicy == "" {
		return &spawnFailure{http.StatusForbidden, "approval_restricted",
			fmt.Sprintf("agent %s has a legacy %s launch whose approval posture cannot be reconstructed; relaunch it with current tclaude to record the conservative untrusted posture before spawning a matching child",
				short8(parentConvID), parentHarness)}
	}
	if !harness.ApprovalLineageAllowed(parentHarness, parentPolicy, parentAutoReview,
		childHarness, childPolicy, childAutoReview) {
		msg := fmt.Sprintf("agent %s was launched with %s approval %q (auto-review=%t) and may not spawn a %s child with approval %q (auto-review=%t)",
			short8(parentConvID), parentHarness, parentPolicy, parentAutoReview,
			childHarness, childPolicy, childAutoReview)
		// Name the way out when there is one. The common case is an unresolvable
		// `inherit` child, where "pass --ask-for-approval auto" is the whole fix
		// and the bare denial reads as a dead end.
		if hint := harness.ApprovalLineageDenialHint(childHarness, childPolicy); hint != "" {
			msg += ": " + hint
		}
		return &spawnFailure{http.StatusForbidden, "approval_restricted", msg}
	}
	return nil
}

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
	childHarness = harnessOrDefault(childHarness)
	childPolicy = strings.TrimSpace(childPolicy)
	if !harness.ApprovalLineageAllowed(parentHarness, parentPolicy, parentAutoReview,
		childHarness, childPolicy, childAutoReview) {
		return &spawnFailure{http.StatusForbidden, "approval_restricted",
			fmt.Sprintf("agent %s was launched with %s approval %q (auto-review=%t) and may not spawn a %s child with approval %q (auto-review=%t)",
				short8(parentConvID), parentHarness, parentPolicy, parentAutoReview,
				childHarness, childPolicy, childAutoReview)}
	}
	return nil
}

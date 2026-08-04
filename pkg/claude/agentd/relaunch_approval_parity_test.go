package agentd

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/approvalfixture"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// Every daemon surface that rebuilds a launch must reconstruct exactly what the
// canonical table says — the same table pkg/claude/conv's resume path is held
// to. A daemon surface that pins a blank row to a historical value while the
// CLI re-resolves it (or the reverse) fails here. See TCL-990.
func TestDaemonRelaunchApprovalMatchesTheCanonicalReconstruction(t *testing.T) {
	for i, tc := range approvalfixture.Cases() {
		t.Run(tc.Name, func(t *testing.T) {
			setupTestDB(t)
			convID := fmt.Sprintf("relaunch-approval-parity-%d", i)
			agentID, _, err := db.EnsureAgentForConv(convID, "test")
			require.NoError(t, err)
			recorded := tc.Recorded
			require.NoError(t, db.SetAgentRelaunchProfile(agentID, db.AgentRelaunchProfile{
				Version: db.RelaunchProfileVersion, ApprovalPolicy: &recorded,
			}))
			require.NoError(t, db.SaveSession(&db.SessionRow{
				ID: convID + "-session", ConvID: convID, Cwd: t.TempDir(),
				Harness: tc.Harness, Status: session.StatusIdle,
				ApprovalPolicy: tc.Recorded, ResumeProvenance: "test-proof",
			}))

			policy, autoReview := approvalForRelaunch(convID, tc.Harness)
			assert.Equal(t, tc.Want, policy, "approvalForRelaunch (clone)")
			assert.False(t, autoReview)

			relaunch, err := durableRelaunchConfigForConv(convID)
			require.NoError(t, err)
			assert.Equal(t, tc.Want, relaunch.Approval,
				"durable relaunch profile (reincarnate/restart/dashboard)")

			shared, err := reconstructApproval(tc.Harness, tc.Recorded)
			require.NoError(t, err)
			assert.Equal(t, tc.Want, shared, "the shared daemon entry point")

			if tc.Recorded == "" {
				assert.Equal(t, tc.Want, approvalForHarness(tc.Harness),
					"the absent-posture arm every error fallback lands in")
			}
		})
	}
}

// A blank Codex row must reach reconstruction still blank. The session→profile
// projection used to rewrite it to `untrusted`, which turned "no input was
// recorded" into a durably recorded posture less capable than what current
// config resolves an unrecorded input to.
func TestBlankCodexRowIsProjectedWithoutInventingAPosture(t *testing.T) {
	setupTestDB(t)
	const convID = "codex-blank-projection"
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: convID + "-session", ConvID: convID, Cwd: t.TempDir(),
		Harness: harness.CodexName, Status: session.StatusIdle,
		ResumeProvenance: "test-proof",
	}))

	launch, err := db.SessionLaunchProfileForConv(convID)
	require.NoError(t, err)
	assert.Empty(t, launch.ApprovalPolicy,
		"an absent approval input must stay absent through the projection")

	policy, _ := approvalForRelaunch(convID, harness.CodexName)
	assert.Equal(t, harness.ApprovalNever, policy)
}

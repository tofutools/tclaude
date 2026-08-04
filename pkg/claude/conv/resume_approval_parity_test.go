package conv

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/approvalfixture"
)

// The CLI resume surface must reconstruct exactly what the canonical table
// says — the same table the daemon relaunch surfaces are held to. The two
// disagreeing for a blank row is the divergence TCL-990 exists to remove, and
// this test is what fails when either side drifts.
func TestResumeApprovalStateMatchesTheCanonicalReconstruction(t *testing.T) {
	for _, tc := range approvalfixture.Cases() {
		t.Run(tc.Name, func(t *testing.T) {
			setupTestDB(t)
			const convID = "8f2c1d40-2222-4333-8444-555566667777"
			require.NoError(t, db.SaveSession(&db.SessionRow{
				ID: "resume-parity-source", ConvID: convID,
				Harness: tc.Harness, ApprovalPolicy: tc.Recorded,
			}))

			h, err := harness.Resolve(tc.Harness)
			require.NoError(t, err)
			state, err := resumeApprovalState(h, convID)
			require.NoError(t, err)
			assert.Equal(t, tc.Want, state.Policy)
			assert.Equal(t, tc.Reresolved, state.Reresolved)
			assert.False(t, state.AutoReview)
		})
	}
}

// A re-resolution must be visible rather than silent: the resume says which
// posture it landed on and that current config, not the record, chose it.
func TestDescribeResumedApprovalNamesAReResolution(t *testing.T) {
	h, err := harness.Resolve(harness.DefaultName)
	require.NoError(t, err)

	reresolved := describeResumedApproval(h, resumedApproval{Policy: "auto", Reresolved: true})
	assert.Contains(t, reresolved, "auto")
	assert.Contains(t, reresolved, "no approval posture was recorded")

	recorded := describeResumedApproval(h, resumedApproval{Policy: harness.ClaudePermissionInherit})
	assert.Contains(t, recorded, harness.ClaudePermissionInherit)
	assert.Contains(t, recorded, "recorded")
	assert.NotContains(t, recorded, "current config")

	assert.Empty(t, describeResumedApproval(h, resumedApproval{}),
		"a harness with no launch-time approval posture has nothing to announce")
}

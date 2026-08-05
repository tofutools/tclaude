package harness_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/approvalfixture"
)

// The rule itself. Every resume/relaunch surface asserts against the same
// table, so this is the definition the others are checked against rather than a
// second opinion. See TCL-990.
func TestReconstructApprovalPolicyFollowsTheCanonicalTable(t *testing.T) {
	for _, tc := range approvalfixture.Cases() {
		t.Run(tc.Name, func(t *testing.T) {
			h, err := harness.Resolve(tc.Harness)
			require.NoError(t, err)
			policy, reresolved, err := harness.ReconstructApprovalPolicy(h, tc.Recorded)
			require.NoError(t, err)
			assert.Equal(t, tc.Want, policy)
			assert.Equal(t, tc.Reresolved, reresolved,
				"only an ABSENT recorded input may report a re-resolution")
		})
	}
}

// An input that is not a posture this harness knows must fail loudly. Falling
// back to the default here would let a corrupt record silently become a fresh
// grant, which is the opposite of reusing the recorded input.
func TestReconstructApprovalPolicyRejectsAnUnknownRecordedPosture(t *testing.T) {
	h, err := harness.Resolve(harness.CodexName)
	require.NoError(t, err)
	_, _, err = harness.ReconstructApprovalPolicy(h, "acceptEdits")
	require.Error(t, err)
}

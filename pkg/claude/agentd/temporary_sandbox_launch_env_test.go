package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// A temporary sandbox-off relaunch (sandbox restart unlock, reincarnate) of a
// Codex agent omits every profile value but keeps the launch environment,
// which is not sandbox policy.
func TestTemporarySandboxLaunchSnapshotKeepsCodexLaunchEnvironment(t *testing.T) {
	stable := sandboxpolicy.EmptySnapshot()
	stable.Effective.Environment = []sandboxpolicy.EnvironmentEntry{{Name: "PROFILE_ONLY", Value: "x"}}
	stable.LaunchEnvironment = []sandboxpolicy.EnvironmentEntry{{Name: "OPENAI_BASE_URL", Value: "https://example.test"}}
	stable.LaunchEnvironmentOverrides = []sandboxpolicy.EnvironmentEntry{{Name: "EXPLICIT", Value: "y"}}
	stable.RefreshGroupEnvironment = true
	stable.ResolutionGroupID = 7

	got := temporarySandboxLaunchSnapshot(harness.CodexName, &stable)
	require.NotNil(t, got)
	assert.True(t, got.ProfilesOmitted)
	assert.Empty(t, got.Effective.Environment, "profile values stay omitted")
	assert.Equal(t, stable.LaunchEnvironment, got.LaunchEnvironment)
	assert.Equal(t, stable.LaunchEnvironmentOverrides, got.LaunchEnvironmentOverrides)
	assert.True(t, got.RefreshGroupEnvironment)
	assert.EqualValues(t, 7, got.ResolutionGroupID)
	assert.Equal(t, stable.LaunchEnvironment, sandboxpolicy.EnvironmentForLaunch(got))

	none := temporarySandboxLaunchSnapshot(harness.CodexName, nil)
	require.NotNil(t, none)
	assert.True(t, none.ProfilesOmitted)
	assert.Empty(t, none.LaunchEnvironment)
}

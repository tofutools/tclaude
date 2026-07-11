package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestWaveChoreographyEffectiveSandboxRoundTrip(t *testing.T) {
	setupTestDB(t)
	groupID, err := CreateAgentGroup("staged", "")
	require.NoError(t, err)

	effective, err := sandboxpolicy.Resolve(sandboxpolicy.Scopes{
		Global: &sandboxpolicy.Profile{
			Name: "global-policy",
			Environment: []sandboxpolicy.EnvironmentEntry{
				{Name: "TCL_WAVE_POLICY", Value: "frozen"},
			},
		},
	})
	require.NoError(t, err)
	snapshot := sandboxpolicy.NewSnapshot(effective, []sandboxpolicy.AppliedProfile{
		{Scope: sandboxpolicy.ScopeGlobal, ID: 42, Name: "global-policy"},
	})

	want := &WaveChoreography{
		GroupID:          groupID,
		GroupName:        "staged",
		TemplateName:     "two-waves",
		EffectiveSandbox: &snapshot,
		Waves:            []WaveGroup{{Wave: 0}, {Wave: 1}},
		NextWave:         1,
	}
	require.NoError(t, UpsertWaveChoreography(want))

	got, err := GetWaveChoreography(groupID)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.NotNil(t, got.EffectiveSandbox)
	assert.Equal(t, snapshot, *got.EffectiveSandbox)
	assert.Equal(t, "frozen", got.EffectiveSandbox.Effective.Environment[0].Value)

	// A second write (the runner's persisted cursor/activation path) keeps the
	// same immutable snapshot bytes.
	got.NextWave = 2
	require.NoError(t, UpsertWaveChoreography(got))
	again, err := GetWaveChoreography(groupID)
	require.NoError(t, err)
	require.NotNil(t, again.EffectiveSandbox)
	assert.Equal(t, snapshot, *again.EffectiveSandbox)
}

package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// applyLaunchPostureEnvSteps is the hand-written sequence spawn and resume
// each used to carry before ApplyLaunchPostureEnv. The shared function must
// produce exactly what it produced, for every harness and posture.
func applyLaunchPostureEnvSteps(h *harness.Harness, posture LaunchPostureEnv, env map[string]string) error {
	if err := ApplyClaudeConfigDirEnv(h.Name, env); err != nil {
		return err
	}
	ApplyClaudeResumeEnv(h, env)
	ApplyAutoMemoryEnv(h, posture.AutoMemory, env)
	ApplyAgentViewEnv(h, env)
	ApplyContextFeaturesEnv(h, posture.ContextFeatures, env)
	ApplyAutoCompactWindowEnv(h, posture.AutoCompactWindow, env)
	return nil
}

func TestApplyLaunchPostureEnvMatchesTheStepSequence(t *testing.T) {
	postures := []LaunchPostureEnv{
		{},
		{AutoMemory: true},
		{ContextFeatures: map[string]string{"bundled-skills": harness.ContextFeatureOff, "agent-view": harness.ContextFeatureOn}},
		{AutoCompactWindow: "450000"},
		{AutoMemory: true, ContextFeatures: map[string]string{"artifact": harness.ContextFeatureOn}, AutoCompactWindow: "200000"},
	}
	for _, name := range harness.Names() {
		h, err := harness.Resolve(name)
		require.NoError(t, err, name)
		for _, posture := range postures {
			want := map[string]string{"PRESET": "kept"}
			wantErr := applyLaunchPostureEnvSteps(h, posture, want)
			got := map[string]string{"PRESET": "kept"}
			gotErr := ApplyLaunchPostureEnv(h, posture, got)
			assert.Equal(t, wantErr, gotErr, "%s %+v", name, posture)
			assert.Equal(t, want, got, "%s %+v", name, posture)
		}
	}
}

// The agent view defaults off, and an explicit agent-view=on trim is how it is
// opted back in; that only works because the trims are applied after.
func TestApplyLaunchPostureEnvAgentViewOptInWins(t *testing.T) {
	claude, err := harness.ResolveSpawnable(harness.DefaultName)
	require.NoError(t, err)
	env := map[string]string{}
	require.NoError(t, ApplyLaunchPostureEnv(claude, LaunchPostureEnv{
		ContextFeatures: map[string]string{"agent-view": harness.ContextFeatureOn},
	}, env))
	value, present := env[harness.AgentViewDisableEnvVar]
	assert.True(t, present)
	assert.Equal(t, "", value, "agent-view=on must override the default-off")

	env = map[string]string{}
	require.NoError(t, ApplyLaunchPostureEnv(claude, LaunchPostureEnv{}, env))
	assert.Equal(t, "1", env[harness.AgentViewDisableEnvVar], "agent view defaults off")
}

package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestApplyAgentViewEnvDisablesTheViewForClaudeCode(t *testing.T) {
	claude, err := harness.ResolveSpawnable(harness.DefaultName)
	require.NoError(t, err)

	env := map[string]string{}
	ApplyAgentViewEnv(claude, env)
	assert.Equal(t, "1", env[harness.AgentViewDisableEnvVar])

	// An inherited value must not turn the view back on under a managed pane.
	env = map[string]string{harness.AgentViewDisableEnvVar: "0"}
	ApplyAgentViewEnv(claude, env)
	assert.Equal(t, "1", env[harness.AgentViewDisableEnvVar])
}

// The launch paths call ApplyAgentViewEnv first and ApplyContextFeaturesEnv
// second. That order is the opt-in: only an explicit agent-view=on may clear the
// default, and reordering them would leave the selector claiming "on" while the
// view stays off.
func TestAgentViewContextFeatureOverridesTheDefault(t *testing.T) {
	claude, err := harness.ResolveSpawnable(harness.DefaultName)
	require.NoError(t, err)

	for state, want := range map[string]string{
		harness.ContextFeatureOn:  "",
		harness.ContextFeatureOff: "1",
	} {
		env := map[string]string{}
		ApplyAgentViewEnv(claude, env)
		ApplyContextFeaturesEnv(claude, map[string]string{"agent-view": state}, env)
		got, ok := env[harness.AgentViewDisableEnvVar]
		assert.True(t, ok, state)
		assert.Equal(t, want, got, state)
	}
}

func TestApplyAgentViewEnvNoOpForOtherHarnesses(t *testing.T) {
	codex, err := harness.ResolveSpawnable(harness.CodexName)
	require.NoError(t, err)

	env := map[string]string{}
	ApplyAgentViewEnv(codex, env)
	assert.Empty(t, env)

	assert.NotPanics(t, func() { ApplyAgentViewEnv(nil, map[string]string{}) })
	assert.NotPanics(t, func() { ApplyAgentViewEnv(codex, nil) })
}

// runNew needs tmux and a session row, so the fresh-launch wiring is guarded
// structurally. The agent-view default and the context-feature trims are
// applied by ApplyLaunchPostureEnv (shared with resume); runNew must call it on
// every launch, before BuildEnvExports turns the env into the launch script.
// Inside it, the default must precede the trims, which is the opt-in.
func TestRunNewAppliesTheAgentViewDefault(t *testing.T) {
	fn := guardFunc(guardParse(t, "new.go", ""), "runNew")
	require.NotNil(t, fn, "runNew must exist for this guard to mean anything")

	order := callOrder(fn, "ApplyLaunchPostureEnv", "BuildEnvExports")
	require.Contains(t, order, "ApplyLaunchPostureEnv")
	require.Contains(t, order, "BuildEnvExports")
	assert.Less(t, order["ApplyLaunchPostureEnv"], order["BuildEnvExports"],
		"the posture env must be applied before the env exports are built, or it never reaches the launch script")
	assert.True(t, callIsUnconditional(fn, "ApplyLaunchPostureEnv"),
		"every fresh launch must get the default, not only those down one branch")

	posture := guardFunc(guardParse(t, "launch_posture_env.go", ""), "ApplyLaunchPostureEnv")
	require.NotNil(t, posture, "ApplyLaunchPostureEnv must exist for this guard to mean anything")
	inner := callOrder(posture, "ApplyAgentViewEnv", "ApplyContextFeaturesEnv")
	require.Contains(t, inner, "ApplyAgentViewEnv")
	require.Contains(t, inner, "ApplyContextFeaturesEnv")
	assert.Less(t, inner["ApplyAgentViewEnv"], inner["ApplyContextFeaturesEnv"],
		"the default must come first, or agent-view=on can no longer clear it")
	assert.True(t, callIsUnconditional(posture, "ApplyAgentViewEnv"))
}

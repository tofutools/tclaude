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
// structurally. The order against ApplyContextFeaturesEnv is the opt-in, and the
// order against BuildEnvExports is what gets the value into the launch script.
func TestRunNewAppliesTheAgentViewDefault(t *testing.T) {
	fn := guardFunc(guardParse(t, "new.go", ""), "runNew")
	require.NotNil(t, fn, "runNew must exist for this guard to mean anything")

	order := callOrder(fn, "ApplyAgentViewEnv", "ApplyContextFeaturesEnv", "BuildEnvExports")
	require.Contains(t, order, "ApplyAgentViewEnv")
	require.Contains(t, order, "ApplyContextFeaturesEnv")
	require.Contains(t, order, "BuildEnvExports")

	assert.Less(t, order["ApplyAgentViewEnv"], order["ApplyContextFeaturesEnv"],
		"the default must come first, or agent-view=on can no longer clear it")
	assert.Less(t, order["ApplyContextFeaturesEnv"], order["BuildEnvExports"],
		"both must run before the env exports are built, or neither reaches the launch script")
	assert.True(t, callIsUnconditional(fn, "ApplyAgentViewEnv"),
		"every fresh launch must get the default, not only those down one branch")
}

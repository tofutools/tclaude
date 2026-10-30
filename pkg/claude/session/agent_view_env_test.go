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

func TestApplyAgentViewEnvNoOpForOtherHarnesses(t *testing.T) {
	codex, err := harness.ResolveSpawnable(harness.CodexName)
	require.NoError(t, err)

	env := map[string]string{}
	ApplyAgentViewEnv(codex, env)
	assert.Empty(t, env)

	assert.NotPanics(t, func() { ApplyAgentViewEnv(nil, map[string]string{}) })
	assert.NotPanics(t, func() { ApplyAgentViewEnv(codex, nil) })
}

package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestApplyAutoMemoryEnvSetsBothDirections(t *testing.T) {
	claude, err := harness.ResolveSpawnable(harness.DefaultName)
	require.NoError(t, err)

	// The default posture: memory disabled.
	env := map[string]string{}
	ApplyAutoMemoryEnv(claude, false, env)
	assert.Equal(t, "1", env[harness.AutoMemoryEnvVar])

	// The opt-in posture is written EXPLICITLY as "0" rather than omitted.
	// BuildEnvExports forwards the operator's own os.Environ(), so an operator
	// who exports CLAUDE_CODE_DISABLE_AUTO_MEMORY=1 in their shell must not be
	// able to silently override an agent that opted into memory.
	env = map[string]string{}
	ApplyAutoMemoryEnv(claude, true, env)
	assert.Equal(t, "0", env[harness.AutoMemoryEnvVar])
}

func TestApplyAutoMemoryEnvOverridesInheritedValue(t *testing.T) {
	claude, err := harness.ResolveSpawnable(harness.DefaultName)
	require.NoError(t, err)

	// A pre-seeded value (the shape an inherited/sandbox env would take) is
	// replaced by the resolved posture, not merely left alone.
	env := map[string]string{harness.AutoMemoryEnvVar: "1"}
	ApplyAutoMemoryEnv(claude, true, env)
	assert.Equal(t, "0", env[harness.AutoMemoryEnvVar])
}

func TestApplyAutoMemoryEnvNoOpForHarnessWithoutMemory(t *testing.T) {
	codex, err := harness.ResolveSpawnable(harness.CodexName)
	require.NoError(t, err)

	env := map[string]string{}
	ApplyAutoMemoryEnv(codex, false, env)
	assert.NotContains(t, env, harness.AutoMemoryEnvVar, "no auto-memory switch to set for Codex")

	ApplyAutoMemoryEnv(codex, true, env)
	assert.NotContains(t, env, harness.AutoMemoryEnvVar)
}

func TestApplyAutoMemoryEnvNilSafe(t *testing.T) {
	claude, err := harness.ResolveSpawnable(harness.DefaultName)
	require.NoError(t, err)

	assert.NotPanics(t, func() { ApplyAutoMemoryEnv(nil, false, map[string]string{}) })
	assert.NotPanics(t, func() { ApplyAutoMemoryEnv(claude, false, nil) })
}

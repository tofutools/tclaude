package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// ApplyContextFeaturesEnv is the single seam every env-backed startup-context
// trim passes through on the way to every launched AND resumed pane, so it gets
// the same coverage as its sibling ApplyAutoMemoryEnv.

func TestApplyContextFeaturesEnvSetsBothDirections(t *testing.T) {
	claude, err := harness.ResolveSpawnable(harness.DefaultName)
	require.NoError(t, err)

	env := map[string]string{}
	ApplyContextFeaturesEnv(claude, map[string]string{
		"bundled-skills": harness.ContextFeatureOff,
		"artifact":       harness.ContextFeatureOn,
	}, env)

	assert.Equal(t, "1", env["CLAUDE_CODE_DISABLE_BUNDLED_SKILLS"], "trim disables")
	// Keep is written EXPLICITLY as the empty string rather than omitted — see
	// below for why that matters.
	value, present := env["CLAUDE_CODE_DISABLE_ARTIFACT"]
	assert.True(t, present, "keep must still pin the variable")
	assert.Equal(t, "", value)
}

func TestApplyContextFeaturesEnvKeepOverridesOperatorExport(t *testing.T) {
	claude, err := harness.ResolveSpawnable(harness.DefaultName)
	require.NoError(t, err)

	// THE reason keep writes a value at all. BuildEnvExports forwards the
	// operator's own os.Environ(), so an operator who exports
	// CLAUDE_CODE_DISABLE_ARTIFACT=1 in their shell would otherwise silently
	// override an agent that explicitly asked to keep the feature. A pre-seeded
	// value (the shape an inherited env takes) must be REPLACED, not left alone.
	env := map[string]string{"CLAUDE_CODE_DISABLE_ARTIFACT": "1"}
	ApplyContextFeaturesEnv(claude, map[string]string{"artifact": harness.ContextFeatureOn}, env)
	assert.Equal(t, "", env["CLAUDE_CODE_DISABLE_ARTIFACT"],
		"an explicit keep must beat an inherited disable")
}

func TestApplyContextFeaturesEnvLeavesDefaultAlone(t *testing.T) {
	claude, err := harness.ResolveSpawnable(harness.DefaultName)
	require.NoError(t, err)

	// Default means "tclaude injects nothing", so an operator's own setting stays
	// in force. A resolved map never contains a default entry, but an inherited
	// variable for an UNSTEERED feature must survive untouched.
	env := map[string]string{"CLAUDE_CODE_DISABLE_ARTIFACT": "1"}
	ApplyContextFeaturesEnv(claude, map[string]string{"cron": harness.ContextFeatureOff}, env)
	assert.Equal(t, "1", env["CLAUDE_CODE_DISABLE_ARTIFACT"],
		"an unsteered feature must keep the operator's own value")
	assert.Equal(t, "1", env["CLAUDE_CODE_DISABLE_CRON"])
}

func TestApplyContextFeaturesEnvSkipsSettingsOnlyTrims(t *testing.T) {
	claude, err := harness.ResolveSpawnable(harness.DefaultName)
	require.NoError(t, err)

	// claude-ai-connectors has no CLAUDE_CODE_DISABLE_* twin; it rides the
	// --settings payload instead, so this seam must contribute nothing for it.
	env := map[string]string{}
	ApplyContextFeaturesEnv(claude, map[string]string{
		"claude-ai-connectors": harness.ContextFeatureOff,
	}, env)
	assert.Empty(t, env, "a settings-only trim must add no environment variables")
}

func TestApplyContextFeaturesEnvNoOpForHarnessWithoutTrims(t *testing.T) {
	codex, err := harness.ResolveSpawnable(harness.CodexName)
	require.NoError(t, err)

	env := map[string]string{}
	ApplyContextFeaturesEnv(codex, map[string]string{"bundled-skills": harness.ContextFeatureOff}, env)
	assert.Empty(t, env, "Codex has no steerable startup context, so nothing is injected")
}

func TestApplyContextFeaturesEnvEmptyMapInjectsNothing(t *testing.T) {
	claude, err := harness.ResolveSpawnable(harness.DefaultName)
	require.NoError(t, err)

	env := map[string]string{}
	ApplyContextFeaturesEnv(claude, nil, env)
	assert.Empty(t, env)
	ApplyContextFeaturesEnv(claude, map[string]string{}, env)
	assert.Empty(t, env, "an agent that trims nothing must launch with a pristine env")
}

func TestApplyContextFeaturesEnvNilSafe(t *testing.T) {
	claude, err := harness.ResolveSpawnable(harness.DefaultName)
	require.NoError(t, err)

	trims := map[string]string{"bundled-skills": harness.ContextFeatureOff}
	assert.NotPanics(t, func() { ApplyContextFeaturesEnv(nil, trims, map[string]string{}) })
	assert.NotPanics(t, func() { ApplyContextFeaturesEnv(claude, trims, nil) })
}

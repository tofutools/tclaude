package harness

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestContextFeatureCatalogIsWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, f := range ContextFeatures() {
		assert.NotEmpty(t, f.Slug, "every feature needs a slug")
		assert.False(t, seen[f.Slug], "duplicate slug %q", f.Slug)
		seen[f.Slug] = true
		assert.NotEmpty(t, f.Label, "%s: needs a UI label", f.Slug)
		assert.NotEmpty(t, f.Descr, "%s: needs a description", f.Slug)
		// Exactly one delivery mechanism. Both would emit the trim twice; neither
		// would render a row the operator can toggle but that does nothing.
		bothSet := f.EnvVar != "" && f.SettingsKey != ""
		neitherSet := f.EnvVar == "" && f.SettingsKey == ""
		assert.False(t, bothSet, "%s: has both an env var and a settings key", f.Slug)
		assert.False(t, neitherSet, "%s: has no delivery mechanism", f.Slug)
	}
	assert.Equal(t, ContextFeatureSlugs(), slugsOf(ContextFeatures()),
		"ContextFeatureSlugs must mirror the catalog order")
}

func slugsOf(features []ContextFeature) []string {
	out := make([]string, 0, len(features))
	for _, f := range features {
		out = append(out, f.Slug)
	}
	return out
}

func TestContextFeaturesMutationDoesNotReachTheRegistry(t *testing.T) {
	copied := ContextFeatures()
	require.NotEmpty(t, copied)
	copied[0].Slug = "tampered"
	assert.NotEqual(t, "tampered", ContextFeatures()[0].Slug)
}

func TestResolveContextFeaturesDropsDefaultsAndNormalizes(t *testing.T) {
	h, err := ResolveSpawnable(DefaultName)
	require.NoError(t, err)

	got, err := ResolveContextFeatures(h, map[string]string{
		"bundled-skills": "OFF",
		" artifact ":     "on",
		"workflows":      "default",
		"cron":           "",
	})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"bundled-skills": ContextFeatureOff,
		"artifact":       ContextFeatureOn,
	}, got, "default/blank states are dropped; slugs and states are normalized")
}

func TestResolveContextFeaturesEmptyIsNilForEveryHarness(t *testing.T) {
	for _, name := range []string{DefaultName, CodexName} {
		h, err := ResolveSpawnable(name)
		require.NoError(t, err, name)
		// An all-default map must be indistinguishable from no map at all, so a
		// profile field round-trips as unset — and so a Codex profile that touched
		// the control and reverted it is not rejected.
		got, err := ResolveContextFeatures(h, map[string]string{"artifact": "default"})
		require.NoError(t, err, name)
		assert.Nil(t, got, name)
	}
}

func TestResolveContextFeaturesRejectsBadInput(t *testing.T) {
	claude, err := ResolveSpawnable(DefaultName)
	require.NoError(t, err)
	codex, err := ResolveSpawnable(CodexName)
	require.NoError(t, err)

	_, err = ResolveContextFeatures(claude, map[string]string{"no-such-feature": "off"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown context feature")

	_, err = ResolveContextFeatures(claude, map[string]string{"artifact": "maybe"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported context-feature state")

	// A real trim asked of a harness with no steerable startup context is an
	// error, not a silent drop — the mistake must surface at the spawn boundary.
	_, err = ResolveContextFeatures(codex, map[string]string{"artifact": "off"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no steerable startup-context features")
}

func TestContextFeatureEnvDirections(t *testing.T) {
	env := ContextFeatureEnv(map[string]string{
		"bundled-skills":       ContextFeatureOff,
		"artifact":             ContextFeatureOn,
		"claude-ai-connectors": ContextFeatureOff, // settings-only: no env var
	})
	assert.Equal(t, "1", env["CLAUDE_CODE_DISABLE_BUNDLED_SKILLS"], "off disables")
	// ON writes an EMPTY value rather than omitting the variable, so an operator's
	// own exported "=1" cannot override an agent that asked to keep the feature.
	// Empty reads as "not disabled" under both a truthiness test and an equality
	// test against "1"; "0" would only be safe under the latter.
	value, present := env["CLAUDE_CODE_DISABLE_ARTIFACT"]
	assert.True(t, present, "on must still pin the variable")
	assert.Equal(t, "", value)
	assert.NotContains(t, env, "disableClaudeAiConnectors", "settings-only trims never ride env")

	assert.Nil(t, ContextFeatureEnv(nil))
	assert.Nil(t, ContextFeatureEnv(map[string]string{"claude-ai-connectors": ContextFeatureOff}),
		"a settings-only trim contributes no env at all")
}

func TestContextFeatureSettingsEmitsBothDirections(t *testing.T) {
	assert.Equal(t, map[string]bool{"disableClaudeAiConnectors": true},
		ContextFeatureSettings(map[string]string{"claude-ai-connectors": ContextFeatureOff}))
	// An explicit false is what lets a per-spawn "keep it" beat an operator-level
	// disable, since the payload merges OVER their settings.json.
	assert.Equal(t, map[string]bool{"disableClaudeAiConnectors": false},
		ContextFeatureSettings(map[string]string{"claude-ai-connectors": ContextFeatureOn}))
	assert.Nil(t, ContextFeatureSettings(map[string]string{"artifact": ContextFeatureOff}),
		"an env-backed trim contributes no settings key")
}

func TestParseContextFeaturesCLISpellings(t *testing.T) {
	got, err := ParseContextFeatures(" bundled-skills , Artifact=on ,workflows=off, ")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"bundled-skills": "off", // a bare slug means off, the common intent
		"artifact":       "on",
		"workflows":      "off",
	}, got)

	empty, err := ParseContextFeatures("   ")
	require.NoError(t, err)
	assert.Nil(t, empty)

	_, err = ParseContextFeatures("artifact=on,artifact=off")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "listed twice")

	_, err = ParseContextFeatures("=off")
	require.Error(t, err)
}

func TestFormatContextFeaturesIsSortedAndStable(t *testing.T) {
	rendered := FormatContextFeatures(map[string]string{
		"workflows": "off", "artifact": "on", "bundled-skills": "off",
	})
	assert.Equal(t, "artifact=on, bundled-skills=off, workflows=off", rendered)
	assert.Empty(t, FormatContextFeatures(nil))
}

func TestContextFeatureRoundTripThroughTheCLISpelling(t *testing.T) {
	h, err := ResolveSpawnable(DefaultName)
	require.NoError(t, err)
	// The daemon renders a resolved map with FormatContextFeatures and the forked
	// `session new` parses it back with ParseContextFeatures. That handoff must be
	// lossless or an agent silently launches with different trims than resolved.
	original := map[string]string{
		"bundled-skills": ContextFeatureOff,
		"artifact":       ContextFeatureOn,
		"claude-mds":     ContextFeatureOff,
	}
	parsed, err := ParseContextFeatures(FormatContextFeatures(original))
	require.NoError(t, err)
	resolved, err := ResolveContextFeatures(h, parsed)
	require.NoError(t, err)
	assert.Equal(t, original, resolved)
}

func TestClaudeSettingsCarriesSettingsOnlyContextTrims(t *testing.T) {
	// The spawner emits `--settings` at most once, so a settings-only trim must
	// join the SAME payload as the sandbox block and the ask-timeout.
	payload := claudeSettingsJSON(SpawnSpec{
		ContextFeatures: map[string]string{
			"claude-ai-connectors": ContextFeatureOff,
			"bundled-skills":       ContextFeatureOff,
		},
	})
	assert.JSONEq(t, `{"disableClaudeAiConnectors":true}`, payload,
		"only the settings-only trim lands here; bundled-skills rides the env")

	assert.Empty(t, claudeSettingsJSON(SpawnSpec{
		ContextFeatures: map[string]string{"bundled-skills": ContextFeatureOff},
	}), "an env-only trim must not force an otherwise-empty --settings payload")
}

func TestSupportsContextFeaturesIsClaudeOnly(t *testing.T) {
	claude, err := ResolveSpawnable(DefaultName)
	require.NoError(t, err)
	assert.True(t, claude.SupportsContextFeatures())
	assert.True(t, claude.CanContextFeatures())

	codex, err := ResolveSpawnable(CodexName)
	require.NoError(t, err)
	assert.False(t, codex.SupportsContextFeatures())
	assert.False(t, codex.CanContextFeatures())

	var nilHarness *Harness
	assert.False(t, nilHarness.SupportsContextFeatures())
}

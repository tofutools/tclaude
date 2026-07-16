package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSpawnHarnessRulesGroupOverridesGlobalPerEdge(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, ReplaceSpawnHarnessRules(0, []SpawnHarnessRule{{
		SourceHarness: "claude", TargetHarness: "codex",
		Decision: SpawnHarnessDeny, Reason: "save Codex credits",
	}}))

	rule, scope, found, err := ResolveSpawnHarnessRule(42, "claude", "codex")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "global", scope)
	assert.Equal(t, SpawnHarnessDeny, rule.Decision)
	assert.Equal(t, "save Codex credits", rule.Reason)

	require.NoError(t, ReplaceSpawnHarnessRules(42, []SpawnHarnessRule{{
		SourceHarness: "claude", TargetHarness: "codex", Decision: SpawnHarnessAllow,
	}}))
	rule, scope, found, err = ResolveSpawnHarnessRule(42, "claude", "codex")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, "group", scope)
	assert.Equal(t, SpawnHarnessAllow, rule.Decision)

	// The reverse direction is a distinct edge and defaults to allow.
	rule, scope, found, err = ResolveSpawnHarnessRule(42, "codex", "claude")
	require.NoError(t, err)
	assert.False(t, found)
	assert.Equal(t, "default", scope)
	assert.Equal(t, SpawnHarnessAllow, rule.Decision)
}

func TestSpawnHarnessRulesDenyRequiresReason(t *testing.T) {
	setupTestDB(t)
	err := ReplaceSpawnHarnessRules(0, []SpawnHarnessRule{{
		SourceHarness: "claude", TargetHarness: "codex", Decision: SpawnHarnessDeny,
	}})
	assert.EqualError(t, err, "a deny rule requires a reason")
}

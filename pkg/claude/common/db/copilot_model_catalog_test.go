package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCopilotModelCatalogReplaceAndFreshLookup(t *testing.T) {
	setupTestDB(t)
	fetchedAt := time.Date(2026, 8, 9, 8, 0, 0, 0, time.UTC)
	require.NoError(t, ReplaceCopilotModelCatalog([]CopilotModelCatalogEntry{
		{ModelID: "claude-haiku-4.5", MaxContextWindowTokens: 144_000,
			MaxPromptTokens: 128_000, MaxOutputTokens: 32_000, RawJSON: `{"id":"claude-haiku-4.5"}`},
		{ModelID: "gpt-5.6-sol", MaxContextWindowTokens: 400_000,
			MaxPromptTokens: 272_000, MaxOutputTokens: 128_000},
	}, fetchedAt))

	limit, fresh, err := FreshCopilotModelPromptLimit(
		"claude-haiku-4.5", fetchedAt.Add(23*time.Hour), 24*time.Hour)
	require.NoError(t, err)
	assert.True(t, fresh)
	assert.Equal(t, int64(128_000), limit)

	limit, fresh, err = FreshCopilotModelPromptLimit(
		"claude-haiku-4.5", fetchedAt.Add(24*time.Hour), 24*time.Hour)
	require.NoError(t, err)
	assert.False(t, fresh)
	assert.Zero(t, limit)

	require.NoError(t, ReplaceCopilotModelCatalog([]CopilotModelCatalogEntry{
		{ModelID: "gpt-5.6-sol", MaxPromptTokens: 300_000},
	}, fetchedAt.Add(time.Hour)))
	_, fresh, err = FreshCopilotModelPromptLimit(
		"claude-haiku-4.5", fetchedAt.Add(time.Hour), 24*time.Hour)
	require.NoError(t, err)
	assert.False(t, fresh, "atomic replacement removes models absent from the new catalog")
}

func TestCopilotModelCatalogFallsBackToContextWindowLimit(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC()
	require.NoError(t, ReplaceCopilotModelCatalog([]CopilotModelCatalogEntry{
		{ModelID: "model-without-prompt-limit", MaxContextWindowTokens: 144_000},
	}, now))

	limit, fresh, err := FreshCopilotModelPromptLimit(
		"model-without-prompt-limit", now.Add(time.Hour), 24*time.Hour)
	require.NoError(t, err)
	assert.True(t, fresh)
	assert.Equal(t, int64(144_000), limit)
}

func TestCopilotModelCatalogRejectsEmptyReplacementWithoutClearing(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC()
	require.NoError(t, ReplaceCopilotModelCatalog([]CopilotModelCatalogEntry{
		{ModelID: "claude-haiku-4.5", MaxPromptTokens: 128_000},
	}, now))
	require.Error(t, ReplaceCopilotModelCatalog(nil, now.Add(time.Hour)))

	limit, fresh, err := FreshCopilotModelPromptLimit(
		"claude-haiku-4.5", now.Add(time.Hour), 24*time.Hour)
	require.NoError(t, err)
	assert.True(t, fresh)
	assert.Equal(t, int64(128_000), limit)
}

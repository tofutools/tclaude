package db

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGlobalDefaultSpawnProfile_StableIDSurvivesRename(t *testing.T) {
	setupTestDB(t)
	id, err := CreateSpawnProfile(&SpawnProfile{Name: "before", Harness: "codex"})
	require.NoError(t, err)
	require.NoError(t, SetDashboardProfileRef(
		DashboardDefaultSpawnProfileNamePrefKey,
		DashboardDefaultSpawnProfileIDPrefKey,
		"before", id))
	require.NoError(t, UpdateSpawnProfile(&SpawnProfile{ID: id, Name: "after", Harness: "codex"}))

	got, err := GlobalDefaultSpawnProfile()
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "after", got.Name)
}

func TestGlobalDefaultSpawnProfile_LegacyNameFallback(t *testing.T) {
	setupTestDB(t)
	_, err := CreateSpawnProfile(&SpawnProfile{Name: "legacy", Harness: "claude"})
	require.NoError(t, err)
	require.NoError(t, SetDashboardPref(DashboardDefaultSpawnProfileNamePrefKey, "legacy"))

	got, err := GlobalDefaultSpawnProfile()
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "legacy", got.Name)
}

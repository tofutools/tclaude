package agent

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// SpawnNameFixtureRow is one row of testdata/spawn_names.json, the one table
// the spawn-name gate and normalizer are pinned to at every site: this CLI,
// the daemon (agentd) and the dashboard JS (jstest/spawn-name-fixture).
type spawnNameFixtureRow struct {
	In         string `json:"in"`
	Valid      bool   `json:"valid"`
	Normalized string `json:"normalized"`
}

func loadSpawnNameFixture(t *testing.T) []spawnNameFixtureRow {
	t.Helper()
	data, err := os.ReadFile("testdata/spawn_names.json")
	require.NoError(t, err)
	var rows []spawnNameFixtureRow
	require.NoError(t, json.Unmarshal(data, &rows))
	require.NotEmpty(t, rows)
	return rows
}

func TestSpawnName_MatchesSharedFixture(t *testing.T) {
	for _, r := range loadSpawnNameFixture(t) {
		assert.Equal(t, r.Valid, isValidSpawnName(r.In), "isValidSpawnName(%q)", r.In)
		assert.Equal(t, r.Normalized, NormalizeSpawnName(r.In), "NormalizeSpawnName(%q)", r.In)
		assert.True(t, isValidSpawnName(NormalizeSpawnName(r.In)), "normalized %q must be valid", r.In)
	}
}

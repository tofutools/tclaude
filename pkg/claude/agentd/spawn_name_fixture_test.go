package agentd

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agent"
)

// The daemon's spawn-name gate agrees with the shared fixture the CLI and the
// dashboard JS are pinned to (jstest/spawn-name-fixture.json).
func TestSpawnName_DaemonMatchesSharedFixture(t *testing.T) {
	data, err := os.ReadFile("jstest/spawn-name-fixture.json")
	require.NoError(t, err)
	var rows []struct {
		In         string `json:"in"`
		Valid      bool   `json:"valid"`
		Normalized string `json:"normalized"`
	}
	require.NoError(t, json.Unmarshal(data, &rows))
	require.NotEmpty(t, rows)
	for _, r := range rows {
		assert.Equal(t, r.Valid, isValidSpawnName(r.In), "isValidSpawnName(%q)", r.In)
		assert.Equal(t, r.Normalized, agent.NormalizeSpawnName(r.In), "agent.NormalizeSpawnName(%q)", r.In)
	}
}

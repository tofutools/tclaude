package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario: spawning an agent records the verbatim spawn request onto the new
// actor's agents.initial_spawn_config — the durable, agent-level "what was this
// spawned with" record (JOH-334). It is write-only by design: tclaude never
// reads it back (resume reads live state), so this test reads the column
// directly to assert the write fired, including the [1m] window selection that
// motivated it.
func TestSpawn_RecordsInitialSpawnConfigVerbatim(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	cwd := t.TempDir()
	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":   "worker",
		"role":   "builder",
		"cwd":    cwd,
		"model":  "opus[1m]",
		"effort": "high",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	require.NotEmpty(t, spawn.ConvID, "spawn returned a conv-id")

	agentID, err := db.AgentIDForConv(spawn.ConvID)
	require.NoError(t, err)
	require.NotEmpty(t, agentID, "spawn must mint an actor")

	// No production reader for this column (write-only by design), so read it
	// straight from the row.
	d, err := db.Open()
	require.NoError(t, err)
	var cfg string
	require.NoError(t, d.QueryRow(
		`SELECT initial_spawn_config FROM agents WHERE agent_id = ?`, agentID).Scan(&cfg))
	require.NotEmpty(t, cfg, "spawn config must be recorded")

	// The column holds the agent.SpawnRequest JSON verbatim — the request as
	// sent, NOT the server-resolved params (so the [1m] alias survives intact).
	var req agent.SpawnRequest
	require.NoErrorf(t, json.Unmarshal([]byte(cfg), &req),
		"stored config must be SpawnRequest JSON; got %s", cfg)
	assert.Equal(t, "opus[1m]", req.Model, "verbatim model selection incl. the 1M variant")
	assert.Equal(t, "high", req.Effort)
	assert.Equal(t, "worker", req.Name)
	assert.Equal(t, "builder", req.Role)
}

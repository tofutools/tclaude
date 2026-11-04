package db

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// A re-pin reads the stored snapshot, then writes a replacement: it must never
// overwrite a snapshot a relaunch recorded in between.
func TestReplaceAgentEffectiveSandboxConfigRefusesAChangedRecord(t *testing.T) {
	setupTestDB(t)
	agentID, _, err := EnsureAgentForConv("launch-record-conv", "spawn")
	require.NoError(t, err)
	require.NoError(t, SetAgentInitialSpawnConfig(agentID, `{"harness_config":"read"}`))
	first := sandboxpolicy.EmptySnapshot()
	require.NoError(t, SetAgentEffectiveSandboxConfig(agentID, &first))

	records, err := ListActiveAgentLaunchRecords()
	require.NoError(t, err)
	require.Len(t, records, 1)
	read := records[0]
	assert.Equal(t, `{"harness_config":"read"}`, read.InitialSpawnConfig)

	relaunched := sandboxpolicy.EmptySnapshot()
	relaunched.Effective.HarnessConfig = sandboxpolicy.HarnessConfigAccessWrite
	require.NoError(t, SetAgentEffectiveSandboxConfig(agentID, &relaunched))

	repinned := sandboxpolicy.EmptySnapshot()
	repinned.Effective.HarnessConfig = sandboxpolicy.HarnessConfigAccessRead
	require.ErrorIs(t, ReplaceAgentEffectiveSandboxConfig(agentID, read.EffectiveSandboxRaw, &repinned),
		ErrAgentLaunchRecordChanged)
	stored, err := AgentEffectiveSandboxConfigForConv("launch-record-conv")
	require.NoError(t, err)
	assert.Equal(t, sandboxpolicy.HarnessConfigAccessWrite, stored.Effective.HarnessConfig, "the relaunch's record stands")

	records, err = ListActiveAgentLaunchRecords()
	require.NoError(t, err)
	require.NoError(t, ReplaceAgentEffectiveSandboxConfig(agentID, records[0].EffectiveSandboxRaw, &repinned))
	stored, err = AgentEffectiveSandboxConfigForConv("launch-record-conv")
	require.NoError(t, err)
	assert.Equal(t, sandboxpolicy.HarnessConfigAccessRead, stored.Effective.HarnessConfig)
}

// Retired agents are skipped in the query itself, and that query is served by
// idx_agents_active, so it never walks an install's retired history.
func TestAgentLaunchRecordsSkipRetiredAgents(t *testing.T) {
	setupTestDB(t)
	snap := sandboxpolicy.EmptySnapshot()
	record := func(conv string) string {
		agentID, _, err := EnsureAgentForConv(conv, "spawn")
		require.NoError(t, err)
		require.NoError(t, SetAgentInitialSpawnConfig(agentID, `{"harness_config":"read"}`))
		require.NoError(t, SetAgentEffectiveSandboxConfig(agentID, &snap))
		return agentID
	}
	active, retired := record("active-conv"), record("retired-conv")
	ok, err := RetireAgentByID(retired, "test", "done")
	require.NoError(t, err)
	require.True(t, ok)

	ids := func(recs []AgentLaunchRecord) map[string]bool {
		out := map[string]bool{}
		for _, r := range recs {
			out[r.AgentID] = r.Retired
		}
		return out
	}
	recs, err := ListActiveAgentLaunchRecords()
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{active: false}, ids(recs))
	recs, err = ListAgentLaunchRecords(true)
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{active: false, retired: true}, ids(recs), "the opt-in lists retired agents, marked")

	got, err := ActiveAgentLaunchRecord(active)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "active-conv", got.ConvID)
	got, err = ActiveAgentLaunchRecord(retired)
	require.NoError(t, err)
	assert.Nil(t, got, "a retired agent has no active launch record")
}

func TestActiveAgentScansUseTheActiveIndex(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	for _, q := range []struct {
		name  string
		query string
		args  []any
	}{
		{"launch records", agentLaunchRecordsQuery(false), nil},
		{"spawn copies of", agentsWithSpawnCopyOfQuery, []any{1}},
	} {
		name := q.name
		rows, err := d.Query(`EXPLAIN QUERY PLAN `+q.query, q.args...)
		require.NoError(t, err, name)
		var plan []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			require.NoError(t, rows.Scan(&id, &parent, &notused, &detail))
			plan = append(plan, detail)
		}
		require.NoError(t, rows.Close())
		assert.Contains(t, strings.Join(plan, "\n"), "idx_agents_active", "%s: %v", name, plan)
	}
}

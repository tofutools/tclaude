package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// The dashboard snapshot names, on each group member, the agent that spawned
// it (spawned_by, from agent_lineage), so the phone's Agents view can nest a
// worker under its lead. An agent a human added has none.
func TestDashboardSnapshot_MemberSpawnedBy(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	f.HaveGroup("alpha")
	const lead = "spawnedby-lead-aaaa-bbbb-cccc-000000000001"
	haveSpawnCapableMember(t, f, "alpha", lead)
	require.NoError(t, db.GrantAgentPermission(lead, agentd.PermGroupsMembersSpawn, "test"))
	worker := f.AsAgent(lead).SpawnWith("alpha", map[string]any{"name": "spawnedby-worker"})
	require.Equalf(t, http.StatusOK, worker.Code, "worker spawn: %s", worker.Raw)
	require.NotEmpty(t, worker.ConvID)
	const human = "spawnedby-human-aaaa-bbbb-cccc-000000000001"
	f.HaveMember("alpha", human)

	leadID, err := db.AgentIDForConv(lead)
	require.NoError(t, err)

	rec := testharness.Serve(agentd.BuildDashboardHandlerForTest(), testharness.JSONRequest(t, http.MethodGet, "/api/snapshot", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var snap struct {
		Groups []struct {
			Name    string `json:"name"`
			Members []struct {
				ConvID    string  `json:"conv_id"`
				SpawnedBy *string `json:"spawned_by"`
			} `json:"members"`
		} `json:"groups"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &snap))
	spawnedBy := map[string]*string{}
	for _, g := range snap.Groups {
		if g.Name != "alpha" {
			continue
		}
		for _, m := range g.Members {
			spawnedBy[m.ConvID] = m.SpawnedBy
		}
	}
	require.Contains(t, spawnedBy, worker.ConvID)
	require.NotNil(t, spawnedBy[worker.ConvID], "the worker names its spawner")
	assert.Equal(t, leadID, *spawnedBy[worker.ConvID])
	require.Contains(t, spawnedBy, lead)
	assert.Nil(t, spawnedBy[lead], "the lead was added, not spawned by an agent: the field is left out")
	require.Contains(t, spawnedBy, human)
	assert.Nil(t, spawnedBy[human])
}

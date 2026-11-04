package db

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateV304toV305_AddsColumnIdempotently(t *testing.T) {
	setupTestDB(t)
	d, err := Open()
	require.NoError(t, err)
	mustExec(t, d, `ALTER TABLE agents DROP COLUMN launch_layers`)
	mustExec(t, d, `UPDATE schema_version SET version = 304`)
	mustExec(t, d, `INSERT INTO agents (agent_id, current_conv_id, created_at)
		VALUES ('agt_existing', 'conv-existing', 1782691200000000000)`)

	require.NoError(t, migrateV304toV305(d))
	require.NoError(t, migrateV304toV305(d), "a re-run is a clean no-op")

	layers, err := AgentLaunchLayersFor("agt_existing")
	require.NoError(t, err)
	assert.Nil(t, layers, "an existing agent reads as pre-change")
	var ver int
	require.NoError(t, d.QueryRow(`SELECT version FROM schema_version`).Scan(&ver))
	assert.Equal(t, 305, ver)
}

func TestReplaceAgentSpawnCopyKeepsOtherLayers(t *testing.T) {
	setupTestDB(t)
	agentID, err := AllocateAgent("conv-layers", "spawn")
	require.NoError(t, err)
	at := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	explicit := map[string]ExplicitLaunchValue{"model": {
		Fields: map[string]json.RawMessage{"Model": json.RawMessage(`"opus"`)}, Provenance: ExplicitAtSpawn, SetAt: at}}
	overrides := []RelaunchPolicyOverride{{Field: "group_env", Mode: "reuse", Source: "explicit"}}
	require.NoError(t, SetAgentLaunchLayers(agentID, &AgentLaunchLayers{
		SpawnCopy: &AgentSpawnCopy{CopiedAt: at, Tiers: []SpawnCopyTier{
			{Tier: SpawnCopyTierNamed, ProfileID: 7, ProfileName: "dev", Profile: json.RawMessage(`{"name":"dev","model":"opus"}`)}}},
		Explicit:          explicit,
		RelaunchOverrides: overrides,
	}))

	later := at.Add(time.Hour)
	require.NoError(t, ReplaceAgentSpawnCopy(agentID, AgentSpawnCopy{CopiedAt: later, Tiers: []SpawnCopyTier{
		{Tier: SpawnCopyTierNamed, ProfileID: 7, ProfileName: "dev", Profile: json.RawMessage(`{"name":"dev","model":"sonnet"}`)}}}))

	got, err := AgentLaunchLayersForConv("conv-layers")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, AgentLaunchLayersVersion, got.Version)
	require.Len(t, got.SpawnCopy.Tiers, 1)
	assert.JSONEq(t, `{"name":"dev","model":"sonnet"}`, string(got.SpawnCopy.Tiers[0].Profile), "the copy is replaced whole")
	assert.True(t, got.SpawnCopy.CopiedAt.Equal(later))
	assert.Equal(t, explicit["model"].Fields, got.Explicit["model"].Fields, "explicit values stay")
	assert.Equal(t, overrides, got.RelaunchOverrides, "per-agent policy overrides stay")

	ids, err := AgentsWithSpawnCopyOf(7)
	require.NoError(t, err)
	assert.Equal(t, []string{agentID}, ids)
	ids, err = AgentsWithSpawnCopyOf(8)
	require.NoError(t, err)
	assert.Empty(t, ids)
}

func TestReplaceAgentSpawnCopySeedsPreChangeAgent(t *testing.T) {
	setupTestDB(t)
	agentID, err := AllocateAgent("conv-old", "spawn")
	require.NoError(t, err)
	require.NoError(t, ReplaceAgentSpawnCopy(agentID, AgentSpawnCopy{CopiedAt: time.Now()}))
	got, err := AgentLaunchLayersFor(agentID)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.NotNil(t, got.SpawnCopy)
	assert.Nil(t, got.Explicit)
	assert.Error(t, ReplaceAgentSpawnCopy("agt_missing", AgentSpawnCopy{}))
}

func TestDecodeAgentLaunchLayersRefusesNewerVersion(t *testing.T) {
	_, err := decodeAgentLaunchLayers(`{"version":99}`)
	assert.Error(t, err)
	got, err := decodeAgentLaunchLayers("")
	require.NoError(t, err)
	assert.Nil(t, got)
}

func TestSetAgentLaunchLayersIfUnsetRecordsOnce(t *testing.T) {
	setupTestDB(t)
	agentID, err := AllocateAgent("conv-once", "spawn")
	require.NoError(t, err)
	first := &AgentLaunchLayers{SpawnCopy: &AgentSpawnCopy{Tiers: []SpawnCopyTier{{Tier: SpawnCopyTierNamed, ProfileID: 1}}}}
	wrote, err := SetAgentLaunchLayersIfUnset(agentID, first)
	require.NoError(t, err)
	assert.True(t, wrote)
	wrote, err = SetAgentLaunchLayersIfUnset(agentID, &AgentLaunchLayers{SpawnCopy: &AgentSpawnCopy{}})
	require.NoError(t, err)
	assert.False(t, wrote, "a second recording is a no-op")
	got, err := AgentLaunchLayersFor(agentID)
	require.NoError(t, err)
	require.Len(t, got.SpawnCopy.Tiers, 1)
}

func TestAgentsWithSpawnCopyOfSkipsMalformedRows(t *testing.T) {
	setupTestDB(t)
	good, err := AllocateAgent("conv-good", "spawn")
	require.NoError(t, err)
	bad, err := AllocateAgent("conv-bad", "spawn")
	require.NoError(t, err)
	require.NoError(t, SetAgentLaunchLayers(good, &AgentLaunchLayers{SpawnCopy: &AgentSpawnCopy{
		Tiers: []SpawnCopyTier{{Tier: SpawnCopyTierNamed, ProfileID: 3}}}}))
	d, err := Open()
	require.NoError(t, err)
	mustExec(t, d, `UPDATE agents SET launch_layers = '{not json' WHERE agent_id = ?`, bad)
	ids, err := AgentsWithSpawnCopyOf(3)
	require.NoError(t, err)
	assert.Equal(t, []string{good}, ids)
}

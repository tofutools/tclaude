package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Trigger and process workers resolve their launch from their own spawn
// profile, then the group's default profile, then the global default, as a
// direct spawn naming that profile does (tcl-a2e791 slice 4, tcl-vciikv).
// Before, the worker's profile took the group default's place, so the group
// default never spoke for these workers.

func spawnedModel(t *testing.T, f *testharness.Flow, conv string) string {
	t.Helper()
	model, ok := f.World.SpawnModel(conv)
	require.Truef(t, ok, "no spawn recorded for %s", conv)
	return model
}

func setGroupDefault(t *testing.T, group string, profile *db.SpawnProfile) {
	t.Helper()
	_, err := db.CreateSpawnProfile(profile)
	require.NoError(t, err)
	_, err = db.SetAgentGroupDefaultProfile(group, profile.Name)
	require.NoError(t, err)
}

func TestManagedGroupDefault_Trigger_NothingConfiguredUnchanged(t *testing.T) {
	f := uniformTriggerGroup(t)
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "reviewer", Harness: "claude", Model: "sonnet"})
	require.NoError(t, err)
	conv := fireUniformTrigger(t, f, "reviewer")
	assert.Equal(t, "sonnet", spawnedModel(t, f, conv))
	assert.Equal(t, allOff, spawnedToggles(t, f, conv))
}

func TestManagedGroupDefault_Trigger_FillsWhatTheWorkerProfileLeavesUnset(t *testing.T) {
	f := uniformTriggerGroup(t)
	setGroupDefault(t, "alpha", &db.SpawnProfile{Name: "group-default", Harness: "claude", Model: "opus", AutoMemory: new(true)})
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "reviewer", Harness: "claude"})
	require.NoError(t, err)
	conv := fireUniformTrigger(t, f, "reviewer")
	assert.Equal(t, "opus", spawnedModel(t, f, conv), "the group default fills the model the worker profile leaves unset")
	assert.True(t, spawnedToggles(t, f, conv).AutoMemory)
	assert.Equal(t, []copyTier{
		{db.SpawnCopyTierNamed, profileID(t, "reviewer"), "reviewer"},
		{db.SpawnCopyTierGroupDefault, profileID(t, "group-default"), "group-default"},
	}, copyTiers(t, conv), "both tiers are recorded in the spawn copy")
}

func TestManagedGroupDefault_Trigger_WorkerProfileWins(t *testing.T) {
	f := uniformTriggerGroup(t)
	setGroupDefault(t, "alpha", &db.SpawnProfile{Name: "group-default", Harness: "claude", Model: "opus", AutoMemory: new(true)})
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "reviewer", Harness: "claude", Model: "sonnet", AutoMemory: new(false)})
	require.NoError(t, err)
	conv := fireUniformTrigger(t, f, "reviewer")
	assert.Equal(t, "sonnet", spawnedModel(t, f, conv))
	assert.False(t, spawnedToggles(t, f, conv).AutoMemory)
}

func TestManagedGroupDefault_Process_FillsWhatTheWorkerProfileLeavesUnset(t *testing.T) {
	f, conv := spawnUniformPerformer(t, &db.SpawnProfile{Name: "dev", Harness: "claude"}, func() {
		setGroupDefault(t, "crew", &db.SpawnProfile{Name: "group-default", Harness: "claude", Model: "opus"})
	})
	assert.Equal(t, "opus", spawnedModel(t, f, conv))
}

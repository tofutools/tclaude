package agentd_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// An agent's launch layers (tcl-a2e791 slice 1b): the spawn profiles that took
// part in its first spawn, copied whole with tier and ID, and the values the
// request set explicitly. Taken once, never touched by session saves.

func launchLayers(t *testing.T, conv string) *db.AgentLaunchLayers {
	t.Helper()
	layers, err := db.AgentLaunchLayersForConv(conv)
	require.NoError(t, err)
	return layers
}

type copyTier struct {
	Tier string
	ID   int64
	Name string
}

func copyTiers(t *testing.T, conv string) []copyTier {
	t.Helper()
	layers := launchLayers(t, conv)
	require.NotNil(t, layers, "a fresh spawn records launch layers")
	require.NotNil(t, layers.SpawnCopy)
	out := []copyTier{}
	for _, tier := range layers.SpawnCopy.Tiers {
		out = append(out, copyTier{tier.Tier, tier.ProfileID, tier.ProfileName})
	}
	return out
}

func profileID(t *testing.T, name string) int64 {
	t.Helper()
	p, err := db.ResolveSpawnProfile(name)
	require.NoError(t, err)
	require.NotNil(t, p, name)
	return p.ID
}

func TestLaunchLayers_Direct_NothingConfigured(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "worker", "initial_message": "Do it."})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	assert.Empty(t, copyTiers(t, spawn.ConvID), "no profile took part")
	assert.Empty(t, launchLayers(t, spawn.ConvID).Explicit, "name and brief are spawn-only, not explicit launch values")
}

func TestLaunchLayers_Direct_RecordsEveryTierAndKeepsTheCopy(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	for _, name := range []string{"named", "group-default", "global-default"} {
		require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{"name": name, "harness": "claude"}).Code)
	}
	require.Equal(t, http.StatusOK, setGroupProfile(t, f, "alpha", "group-default").Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "global-default").Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "worker", "profile": "named", "model": "opus"})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	want := []copyTier{
		{db.SpawnCopyTierNamed, profileID(t, "named"), "named"},
		{db.SpawnCopyTierGroupDefault, profileID(t, "group-default"), "group-default"},
		{db.SpawnCopyTierGlobalDefault, profileID(t, "global-default"), "global-default"},
	}
	assert.Equal(t, want, copyTiers(t, spawn.ConvID))

	explicit := launchLayers(t, spawn.ConvID).Explicit
	require.Contains(t, explicit, "model")
	assert.Equal(t, db.ExplicitAtSpawn, explicit["model"].Provenance)
	assert.JSONEq(t, `"opus"`, string(explicit["model"].Fields["Model"]))
	assert.Contains(t, explicit, "spawn_profile", "naming a profile is an explicit choice")
	assert.NotContains(t, explicit, "identity", "spawn-only values are not relaunch inputs")

	// The copy is taken once: editing the profile, or a stop and resume
	// (which saves new sessions), leaves it as it was.
	before, err := json.Marshal(launchLayers(t, spawn.ConvID))
	require.NoError(t, err)
	named, err := db.ResolveSpawnProfile("named")
	require.NoError(t, err)
	named.Model = "sonnet"
	require.NoError(t, db.UpdateSpawnProfile(named))
	f.Stop(spawn.ConvID, true)
	f.Resume(spawn.ConvID)
	after, err := json.Marshal(launchLayers(t, spawn.ConvID))
	require.NoError(t, err)
	assert.JSONEq(t, string(before), string(after))

	ids, err := db.AgentsWithSpawnCopyOf(profileID(t, "named"))
	require.NoError(t, err)
	assert.Equal(t, []string{spawn.AgentID}, ids, "the agent that keeps its copy of the edited profile")
}

func TestLaunchLayers_TemplateDeploy_RecordsReferencedProfile(t *testing.T) {
	f := newFlow(t)
	createAllOnProfile(t, f, "linked")
	conv := deployUniformTemplate(t, f, map[string]any{"spawn_profile": "linked"})
	assert.Equal(t, []copyTier{{db.SpawnCopyTierNamed, profileID(t, "linked"), "linked"}}, copyTiers(t, conv))
}

func TestLaunchLayers_Trigger_RecordsItsProfileOnce(t *testing.T) {
	f := uniformTriggerGroup(t)
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "reviewer", Harness: "claude"})
	require.NoError(t, err)
	conv := fireUniformTrigger(t, f, "reviewer")
	assert.Equal(t, []copyTier{{db.SpawnCopyTierNamed, profileID(t, "reviewer"), "reviewer"}}, copyTiers(t, conv),
		"the worker's profile also fills the group-default slot today; it is recorded once")
}

func TestLaunchLayers_Scribe_RecordsGlobalDefault(t *testing.T) {
	f := newFlow(t)
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "lean", Harness: "claude"})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "lean").Code)
	conv := summonUniformScribe(t, f)
	assert.Contains(t, copyTiers(t, conv), copyTier{db.SpawnCopyTierGlobalDefault, profileID(t, "lean"), "lean"})
}

// An agent spawned before launch layers existed (an empty column) still picks
// up an edited group environment on resume: group settings re-resolve for
// every agent, and only the spawn copy is seeded from the stored launch shape
// (operator, 2026-11-04).
func TestLaunchLayers_PreChangeAgentReResolvesGroupEnvOnResume(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	_, err := db.SetAgentGroupEnvironment("crew", []db.SandboxEnvironmentEntry{{Name: "GROUP_VALUE", Value: "before"}})
	require.NoError(t, err)
	spawn := f.AsHuman().SpawnWith("crew", map[string]any{"name": "worker"})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	require.Contains(t, resumedEnvironment(t, f, spawn.ConvID),
		db.SandboxEnvironmentEntry{Name: "GROUP_VALUE", Value: "before"})
	require.NoError(t, db.SetAgentLaunchLayers(spawn.AgentID, nil))
	require.Nil(t, launchLayers(t, spawn.ConvID), "reads as a pre-change agent")

	_, err = db.SetAgentGroupEnvironment("crew", []db.SandboxEnvironmentEntry{{Name: "GROUP_VALUE", Value: "after"}})
	require.NoError(t, err)
	f.Stop(spawn.ConvID, true)
	f.Resume(spawn.ConvID)
	assert.Contains(t, resumedEnvironment(t, f, spawn.ConvID),
		db.SandboxEnvironmentEntry{Name: "GROUP_VALUE", Value: "after"})
}

func resumedEnvironment(t *testing.T, f *testharness.Flow, conv string) []db.SandboxEnvironmentEntry {
	t.Helper()
	snapshot, ok := f.World.SpawnSandboxPolicy(conv)
	require.Truef(t, ok, "no launch recorded for %s", conv)
	require.NotNil(t, snapshot)
	return snapshot.LaunchEnvironment
}

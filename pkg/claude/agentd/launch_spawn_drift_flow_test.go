package agentd_test

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/usagerules"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Spawn paths that silently dropped a launch field another path honours
// (tcl-a2e791 slice 2): a process worker's saved prompts, a deployed agent's
// usage-profile links, and context_features off the direct spawn. Each gets a
// "nothing configured is unchanged" pin and a "configured is now honoured" one.

// --- process worker: saved prompts ---

func TestSpawnDrift_ProcessWorker_NoPromptsBriefUnchanged(t *testing.T) {
	_, conv := spawnUniformPerformer(t, &db.SpawnProfile{Name: "dev", Harness: "claude"})
	body := inboxBodies(t, conv)
	assert.Contains(t, body, "Implement the feature and open a PR.")
}

func TestSpawnDrift_ProcessWorker_KeepsProfilePrompts(t *testing.T) {
	_, conv := spawnUniformPerformer(t, &db.SpawnProfile{Name: "dev", Harness: "claude", PromptRefs: []string{"house-rules"}},
		func() {
			require.NoError(t, db.SaveSavedPrompt("", db.SavedPrompt{Name: "house-rules", Body: "HOUSE RULES FOR PERFORMERS"}))
		})
	body := inboxBodies(t, conv)
	assert.Contains(t, body, "HOUSE RULES FOR PERFORMERS", "the profile's saved prompt opens the brief")
	assert.Contains(t, body, "Implement the feature and open a PR.", "the node prompt still follows")
}

// --- template deploy: usage-profile links ---

func agentUsageProfileIDs(t *testing.T, conv string) []int64 {
	t.Helper()
	agentID, err := db.AgentIDForConv(conv)
	require.NoError(t, err)
	assignments, err := db.ListUsageProfileAssignments()
	require.NoError(t, err)
	var ids []int64
	for _, a := range assignments {
		if a.Scope == db.UsageScopeAgent && a.ScopeID == agentID {
			ids = append(ids, a.ProfileID)
		}
	}
	return ids
}

func TestSpawnDrift_TemplateDeploy_NoLinkNoUsageProfile(t *testing.T) {
	f := newFlow(t)
	createAllOnProfile(t, f, "plain")
	conv := deployUniformTemplate(t, f, map[string]any{"spawn_profile": "plain"})
	assert.Empty(t, agentUsageProfileIDs(t, conv))
}

func TestSpawnDrift_TemplateDeploy_InheritsReferencedProfileUsageLink(t *testing.T) {
	f := newFlow(t)
	createAllOnProfile(t, f, "linked")
	sp, err := db.ResolveSpawnProfile("linked")
	require.NoError(t, err)
	usage := &db.UsageProfile{Name: "budget", Rules: []usagerules.Rule{{Name: "cap", Period: usagerules.PeriodLifetime,
		Budget: 5, Steps: []usagerules.Step{{At: 100, Notify: []usagerules.Target{usagerules.TargetOperator}}}}}}
	require.NoError(t, db.CreateUsageProfile(usage))
	require.NoError(t, db.SetUsageProfileAssignments(db.UsageScopeSpawnProfile, strconv.FormatInt(sp.ID, 10),
		[]int64{usage.ID}, "operator"))

	conv := deployUniformTemplate(t, f, map[string]any{"spawn_profile": "linked"})
	assert.Equal(t, []int64{usage.ID}, agentUsageProfileIDs(t, conv),
		"a deployed agent inherits its referenced profile's usage link, as a direct spawn naming it does")
}

// --- context_features off the direct spawn ---

var leanFeatures = map[string]string{"bundled-skills": "off"}

func spawnedFeatures(t *testing.T, f *testharness.Flow, conv string) map[string]string {
	t.Helper()
	got, ok := f.World.SpawnContextFeatures(conv)
	require.Truef(t, ok, "no spawn recorded for %s", conv)
	return got
}

func TestSpawnDrift_Trigger_NoFeaturesUntrimmed(t *testing.T) {
	f := uniformTriggerGroup(t)
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "reviewer", Harness: "claude"})
	require.NoError(t, err)
	assert.Empty(t, spawnedFeatures(t, f, fireUniformTrigger(t, f, "reviewer")))
}

func TestSpawnDrift_Trigger_NamedProfileFeaturesHonoured(t *testing.T) {
	f := uniformTriggerGroup(t)
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "reviewer", Harness: "claude", ContextFeatures: leanFeatures})
	require.NoError(t, err)
	assert.Equal(t, leanFeatures, spawnedFeatures(t, f, fireUniformTrigger(t, f, "reviewer")))
}

func TestSpawnDrift_Process_NamedProfileFeaturesHonoured(t *testing.T) {
	f, conv := spawnUniformPerformer(t, &db.SpawnProfile{Name: "dev", Harness: "claude", ContextFeatures: leanFeatures})
	assert.Equal(t, leanFeatures, spawnedFeatures(t, f, conv))
}

func TestSpawnDrift_Scribe_GlobalProfileFeaturesHonoured(t *testing.T) {
	f := newFlow(t)
	assert.Empty(t, spawnedFeatures(t, f, summonUniformScribe(t, f)), "nothing configured: untrimmed")
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "lean", Harness: "claude", ContextFeatures: leanFeatures})
	require.NoError(t, err)
	require.Equal(t, 200, setGlobalProfile(t, f, "lean").Code)
	assert.Equal(t, leanFeatures, spawnedFeatures(t, f, summonUniformScribe(t, f)))
}

func TestSpawnDrift_TemplateDeploy_GlobalProfileFeaturesHonoured(t *testing.T) {
	f := newFlow(t)
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "lean", Harness: "claude", ContextFeatures: leanFeatures})
	require.NoError(t, err)
	require.Equal(t, 200, setGlobalProfile(t, f, "lean").Code)
	assert.Equal(t, leanFeatures, spawnedFeatures(t, f, deployUniformTemplate(t, f, map[string]any{})))
}

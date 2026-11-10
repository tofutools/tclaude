package agentd_test

import (
	"net/http"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/common/usagerules"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Launch slice 6f (tcl-g2q5wz): a re-applied spawn copy also changes the four
// values a first spawn stores outside the relaunch profile: the group context
// switch and the profile startup context (re-briefed after a compaction), the
// profile environment (merged on every resume) and the usage-profile links.
// Explicit values stay; roles are left alone (they move with their grants).

type outsideState struct {
	include    bool
	context    string
	overrides  []sandboxpolicy.EnvironmentEntry
	usage      []int64
	sandboxRaw string
}

func readOutside(t *testing.T, agentID string) outsideState {
	t.Helper()
	snap, err := db.GetAgentStartupSnapshot(agentID)
	require.NoError(t, err)
	require.NotNil(t, snap)
	rec, err := db.ActiveAgentLaunchRecord(agentID)
	require.NoError(t, err)
	require.NotNil(t, rec)
	require.NotNil(t, rec.EffectiveSandbox)
	assignments, err := db.ListUsageProfileAssignments()
	require.NoError(t, err)
	var usage []int64
	for _, a := range assignments {
		if a.Scope == db.UsageScopeAgent && a.ScopeID == agentID {
			usage = append(usage, a.ProfileID)
		}
	}
	slices.Sort(usage)
	return outsideState{include: snap.IncludeGroupContext, context: snap.ProfileContext,
		overrides: rec.EffectiveSandbox.LaunchEnvironmentOverrides, usage: usage, sandboxRaw: rec.EffectiveSandboxRaw}
}

func usageProfile(t *testing.T, name string) int64 {
	t.Helper()
	u := &db.UsageProfile{Name: name, Rules: []usagerules.Rule{{Name: "cap", Period: usagerules.PeriodLifetime,
		Budget: 5, Steps: []usagerules.Step{{At: 100, Notify: []usagerules.Target{usagerules.TargetOperator}}}}}}
	require.NoError(t, db.CreateUsageProfile(u))
	return u.ID
}

func linkUsage(t *testing.T, profile string, ids ...int64) {
	t.Helper()
	sp, err := db.ResolveSpawnProfile(profile)
	require.NoError(t, err)
	require.NoError(t, db.SetUsageProfileAssignments(db.UsageScopeSpawnProfile, strconv.FormatInt(sp.ID, 10), ids, "operator"))
}

type outsideFixture struct {
	f               *testharness.Flow
	plain, explicit testharness.SpawnResp
	budget, spare   int64
}

func newOutsideFixture(t *testing.T) outsideFixture {
	t.Helper()
	f := newFlow(t)
	f.HaveGroup("alpha")
	_, err := db.SetAgentGroupDefaultContext("alpha", "Group rules.")
	require.NoError(t, err)
	_, err = db.SetAgentGroupEnvironment("alpha", []sandboxpolicy.EnvironmentEntry{{Name: "GROUP", Value: "g"}})
	require.NoError(t, err)
	_, err = db.CreateRole(&db.Role{Name: "copy-reviewer", Brief: "Review carefully."})
	require.NoError(t, err)
	budget, spare := usageProfile(t, "budget"), usageProfile(t, "spare")
	rec := createProfile(t, f, map[string]any{
		"name": "worker", "harness": "claude", "startup_context": "Be brief.", "role_ref": "copy-reviewer",
		"environment": []map[string]string{{"name": "PROF", "value": "one"}},
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	linkUsage(t, "worker", budget)
	plain := spawnStopped(t, f, "alpha", map[string]any{"name": "plain", "profile": "worker"})
	explicit := spawnStopped(t, f, "alpha", map[string]any{"name": "explicit", "profile": "worker",
		"include_group_context": true, "usage_profiles": []string{"spare"},
		"environment": []map[string]string{{"name": "EXPL", "value": "x"}}})
	return outsideFixture{f: f, plain: plain, explicit: explicit, budget: budget, spare: spare}
}

func TestSpawnCopyOutside_SameCopyChangesNothing(t *testing.T) {
	fx := newOutsideFixture(t)
	for _, spawn := range []testharness.SpawnResp{fx.plain, fx.explicit} {
		before := readOutside(t, spawn.AgentID)
		require.Equal(t, "Be brief.\n\n## Role\n\nReview carefully.", before.context, "the profile text, then the role brief")
		layers, err := db.AgentLaunchLayersForConv(spawn.ConvID)
		require.NoError(t, err)
		require.NotNil(t, layers)
		kind, msg := agentd.ApplySpawnCopyForTest(spawn.AgentID, *layers.SpawnCopy)
		require.Empty(t, kind, msg)
		assert.Equal(t, before, readOutside(t, spawn.AgentID), spawn.AgentID)
	}
}

func TestSpawnCopyOutside_ReappliedCopyChangesThemAndKeepsExplicit(t *testing.T) {
	fx := newOutsideFixture(t)
	f := fx.f
	rec := humanReq(t, f, http.MethodPatch, "/v1/spawn-profiles/worker", map[string]any{
		"name": "worker", "harness": "claude", "startup_context": "Be thorough.", "role_ref": "copy-reviewer",
		"include_group_default_context": false,
		"environment":                   []map[string]string{{"name": "PROF", "value": "two"}},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	linkUsage(t, "worker", fx.spare)

	for _, spawn := range []testharness.SpawnResp{fx.plain, fx.explicit} {
		code, res, raw := postReapply(t, f, "worker", spawn.AgentID)
		require.Equal(t, http.StatusOK, code, raw)
		require.Equal(t, "applied", res.Outcome)
	}

	plain := readOutside(t, fx.plain.AgentID)
	assert.False(t, plain.include, "the profile now withholds the group context")
	assert.Equal(t, "Be thorough.\n\n## Role\n\nReview carefully.", plain.context, "only the profile's text changes; the role brief stays")
	assert.Equal(t, []sandboxpolicy.EnvironmentEntry{{Name: "PROF", Value: "two"}}, plain.overrides)
	assert.Equal(t, []int64{fx.spare}, plain.usage, "the profile's current link")

	explicit := readOutside(t, fx.explicit.AgentID)
	assert.True(t, explicit.include, "an explicit include_group_context stays")
	assert.Equal(t, "Be thorough.\n\n## Role\n\nReview carefully.", explicit.context)
	assert.ElementsMatch(t, []sandboxpolicy.EnvironmentEntry{{Name: "PROF", Value: "two"}, {Name: "EXPL", Value: "x"}}, explicit.overrides,
		"explicit entries stay on top of the profile's new ones")
	assert.Equal(t, []int64{fx.spare}, explicit.usage, "usage profiles the spawn named stay its own")

	// The next resume launches with the new environment, over the group's.
	f.AssertResumeSpawned(f.AsHuman().Resume(fx.plain.ConvID))
	policy, ok := f.World.SpawnSandboxPolicy(fx.plain.ConvID)
	require.True(t, ok)
	require.NotNil(t, policy)
	assert.ElementsMatch(t, []db.SandboxEnvironmentEntry{{Name: "GROUP", Value: "g"}, {Name: "PROF", Value: "two"}}, policy.LaunchEnvironment)
}

// A change that cannot be written outside the relaunch profile is reported
// and its old value stays in the copy, so the preview keeps listing it rather
// than showing the agent as up to date.
func TestSpawnCopyOutside_UnwritableChangeIsKeptAndSaid(t *testing.T) {
	fx := newOutsideFixture(t)
	f := fx.f
	// An agent whose launch record has no resolved environment to update.
	rec, err := db.ActiveAgentLaunchRecord(fx.plain.AgentID)
	require.NoError(t, err)
	legacy := *rec.EffectiveSandbox
	legacy.RefreshGroupEnvironment = false
	require.NoError(t, db.SetAgentEffectiveSandboxConfig(fx.plain.AgentID, &legacy))

	patch := humanReq(t, f, http.MethodPatch, "/v1/spawn-profiles/worker", map[string]any{
		"name": "worker", "harness": "claude", "startup_context": "Be brief.", "role_ref": "copy-reviewer",
		"environment": []map[string]string{{"name": "PROF", "value": "two"}},
	})
	require.Equal(t, http.StatusOK, patch.Code, patch.Body.String())
	code, res, raw := postReapply(t, f, "worker", fx.plain.AgentID)
	require.Equal(t, http.StatusOK, code, raw)
	assert.Equal(t, "applied", res.Outcome)
	assert.Contains(t, res.Detail, "kept as before: Profile and explicit environment")

	preview := profileReapplyPreview(t, f, "worker")
	for _, a := range preview.Agents {
		if a.AgentID != fx.plain.AgentID {
			continue
		}
		statuses := map[string]string{}
		for _, field := range a.Fields {
			statuses[field.Key] = field.Status
		}
		assert.Equal(t, map[string]string{"environment": "changes"}, statuses, "the environment still shows as a difference")
	}
}

// An agent re-applying a profile to another agent cannot change that agent's
// environment; the operator can.
func TestSpawnCopyOutside_EnvironmentChangeIsOperatorOnly(t *testing.T) {
	fx := newOutsideFixture(t)
	f := fx.f
	const caller = "reap-1111-2222-3333-7777"
	f.HaveMember("alpha", caller)
	require.NoError(t, db.GrantAgentPermission(caller, agentd.PermProfilesManage, "test"))
	require.NoError(t, db.GrantAgentPermission(caller, agentd.PermAgentMorph, "test"))
	patch := humanReq(t, f, http.MethodPatch, "/v1/spawn-profiles/worker", map[string]any{
		"name": "worker", "harness": "claude", "startup_context": "Be brief.", "role_ref": "copy-reviewer",
		"environment": []map[string]string{{"name": "ANTHROPIC_BASE_URL", "value": "http://example.invalid"}},
	})
	require.Equal(t, http.StatusOK, patch.Code, patch.Body.String())
	before := readOutside(t, fx.plain.AgentID)
	rec := agentReq(t, f, caller, http.MethodPost, "/v1/spawn-profiles/worker/reapply", map[string]any{"agent_id": fx.plain.AgentID})
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "environment_operator_only")
	assert.Equal(t, before, readOutside(t, fx.plain.AgentID), "nothing written")
}

// A usage profile the operator chose for the agent stays when the spawn
// profile's link to it goes, even if the agent had it as a link first.
func TestSpawnCopyOutside_OperatorUsageChoiceSurvivesUnlink(t *testing.T) {
	fx := newOutsideFixture(t)
	f := fx.f
	require.Equal(t, []int64{fx.budget}, readOutside(t, fx.plain.AgentID).usage, "linked from the profile")
	require.NoError(t, db.SetUsageProfileAssignments(db.UsageScopeAgent, fx.plain.AgentID, []int64{fx.budget}, "operator"))
	linkUsage(t, "worker")
	code, _, raw := postReapply(t, f, "worker", fx.plain.AgentID)
	require.Equal(t, http.StatusOK, code, raw)
	assert.Equal(t, []int64{fx.budget}, readOutside(t, fx.plain.AgentID).usage, "the operator's own choice stays")
}

// A profile that stops setting include_group_context leaves the stored value:
// what unset means depends on how the agent was spawned.
func TestSpawnCopyOutside_IncludeGroupContextUnsetIsKept(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	_, err := db.SetAgentGroupDefaultContext("alpha", "Group rules.")
	require.NoError(t, err)
	rec := createProfile(t, f, map[string]any{"name": "quiet", "harness": "claude", "include_group_default_context": false})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	spawn := spawnStopped(t, f, "alpha", map[string]any{"name": "q", "profile": "quiet"})
	snap, err := db.GetAgentStartupSnapshot(spawn.AgentID)
	require.NoError(t, err)
	require.False(t, snap.IncludeGroupContext)

	rec = humanReq(t, f, http.MethodPatch, "/v1/spawn-profiles/quiet", map[string]any{"name": "quiet", "harness": "claude"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	code, res, raw := postReapply(t, f, "quiet", spawn.AgentID)
	require.Equal(t, http.StatusOK, code, raw)
	assert.Contains(t, res.Detail, "kept as before: Include group startup context")
	snap, err = db.GetAgentStartupSnapshot(spawn.AgentID)
	require.NoError(t, err)
	assert.False(t, snap.IncludeGroupContext)
}

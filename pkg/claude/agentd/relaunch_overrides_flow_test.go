package agentd_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Launch slice 11 (tcl-4rd995): per-agent relaunch overrides. A spawn's
// relaunch_policy rows, and its spawn profiles' rows, are copied into the
// agent's launch layers at spawn and override the global table for that
// agent's relaunches. Precedence per row: explicit > profile tiers > global.

func spawnWithRelaunch(t *testing.T, f *testharness.Flow, as *testharness.Flow, name string, extra map[string]any) testharness.SpawnResp {
	t.Helper()
	body := map[string]any{"name": name, "cwd": t.TempDir()}
	for k, v := range extra {
		body[k] = v
	}
	return as.SpawnWith("crew", body)
}

func resumedEnv(t *testing.T, f *testharness.Flow, conv string) (string, testharness.ResumeResp) {
	t.Helper()
	r := resumeForPolicy(t, f, conv)
	policy, ok := f.World.SpawnSandboxPolicy(conv)
	require.True(t, ok)
	return launchEnvValue(t, policy, "CREW_ENV"), r
}

func warningContaining(warnings []string, part string) bool {
	for _, w := range warnings {
		if strings.Contains(w, part) {
			return true
		}
	}
	return false
}

func TestRelaunchOverrides_ExplicitRowOverridesTheGlobalTable(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	setGroupEnv(t, "v1")
	pinned := spawnWithRelaunch(t, f, f.AsHuman(), "pinned", map[string]any{"relaunch_policy": map[string]string{"group_env": "reuse"}})
	require.Equalf(t, http.StatusOK, pinned.Code, "spawn body=%s", pinned.Raw)
	plain := spawnWithRelaunch(t, f, f.AsHuman(), "plain", nil)
	require.Equalf(t, http.StatusOK, plain.Code, "spawn body=%s", plain.Raw)

	setGroupEnv(t, "v2")
	got, r := resumedEnv(t, f, pinned.ConvID)
	assert.Equal(t, "v1", got, "this agent's reuse row keeps the env stored at its last launch")
	assert.True(t, warningContaining(r.Warnings, "relaunch policy for this agent: group_env reuse (this agent)"), "warnings=%v", r.Warnings)
	got, r = resumedEnv(t, f, plain.ConvID)
	assert.Equal(t, "v2", got, "an agent without a row follows the global table (re-resolve)")
	assert.False(t, warningContaining(r.Warnings, "relaunch policy for this agent"), "warnings=%v", r.Warnings)
}

func TestRelaunchOverrides_ProfileRowAndFollowGlobal(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	setGroupEnv(t, "v1")
	rec := profileReq(t, f, http.MethodPost, "/v1/spawn-profiles", map[string]any{
		"name": "pinned-env", "relaunch_policy": map[string]string{"group_env": "reuse"}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	fromProfile := spawnWithRelaunch(t, f, f.AsHuman(), "from-profile", map[string]any{"profile": "pinned-env"})
	require.Equalf(t, http.StatusOK, fromProfile.Code, "spawn body=%s", fromProfile.Raw)
	followGlobal := spawnWithRelaunch(t, f, f.AsHuman(), "follow-global", map[string]any{
		"profile": "pinned-env", "relaunch_policy": map[string]string{"group_env": "inherit"}})
	require.Equalf(t, http.StatusOK, followGlobal.Code, "spawn body=%s", followGlobal.Raw)

	// The row is copied at spawn: removing it from the profile now changes
	// nothing for the agent already spawned.
	rec = profileReq(t, f, http.MethodPatch, "/v1/spawn-profiles/pinned-env", map[string]any{"name": "pinned-env"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	setGroupEnv(t, "v2")
	got, r := resumedEnv(t, f, fromProfile.ConvID)
	assert.Equal(t, "v1", got, "the profile's reuse row, copied at spawn, applies")
	assert.True(t, warningContaining(r.Warnings, "group_env reuse (profile pinned-env)"), "warnings=%v", r.Warnings)
	got, _ = resumedEnv(t, f, followGlobal.ConvID)
	assert.Equal(t, "v2", got, "an explicit inherit follows the global table over the profile's row")
}

func TestRelaunchOverrides_InvalidRowIsRefused(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	for _, rows := range []map[string]string{{"group_env": "chain-wins"}, {"harness": "reuse"}, {"nope": "reuse"}} {
		spawn := spawnWithRelaunch(t, f, f.AsHuman(), "bad", map[string]any{"relaunch_policy": rows})
		assert.Equalf(t, http.StatusBadRequest, spawn.Code, "rows=%v body=%s", rows, spawn.Raw)
		assert.Contains(t, string(spawn.Raw), "invalid_relaunch_policy")
	}
	rec := profileReq(t, f, http.MethodPost, "/v1/spawn-profiles", map[string]any{
		"name": "bad-profile", "relaunch_policy": map[string]string{"group_env": "inherit"}})
	assert.Equal(t, http.StatusBadRequest, rec.Code, "a profile leaves a row out instead of inherit: %s", rec.Body.String())
}

// The gate: an agent spawner may set any row that applies the operator's
// current settings, or one the agent would inherit anyway; a row that keeps
// old settings (reuse, explicit-wins) where it would not, needs
// relaunch-policy.manage. Humans are not gated.
func TestRelaunchOverrides_AgentSpawnerGate(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	parent := spawnWithRelaunch(t, f, f.AsHuman(), "lead", nil)
	require.Equalf(t, http.StatusOK, parent.Code, "spawn body=%s", parent.Raw)
	require.NoError(t, db.GrantAgentPermission(parent.ConvID, agentd.PermGroupsMembersSpawn, "test"))
	agent := f.AsAgent(parent.ConvID)
	spawn := func(name string, extra map[string]any) testharness.SpawnResp {
		return spawnWithRelaunch(t, f, agent, name, extra)
	}
	rows := func(kv ...string) map[string]any {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return map[string]any{"relaunch_policy": m}
	}

	for _, free := range []map[string]any{
		rows("group_env", "re-resolve"),
		rows("sandbox_profile_contents", "inherit"),
		rows("harness_config", "tighten-only"),
		rows("harness_config", "chain-wins"),
		rows("harness_config", "explicit-wins"), // the global default: inherited anyway
	} {
		r := spawn("free", free)
		assert.Equalf(t, http.StatusOK, r.Code, "%v is free: %s", free, r.Raw)
	}

	denied := spawn("denied", rows("group_env", "reuse"))
	assert.Equal(t, http.StatusForbidden, denied.Code, "reuse over the global re-resolve keeps old settings: %s", denied.Raw)
	assert.Contains(t, string(denied.Raw), agentd.PermRelaunchPolicyManage)

	setRelaunchPolicy(t, f, "harness_config", "chain-wins")
	denied = spawn("denied-ew", rows("harness_config", "explicit-wins"))
	assert.Equal(t, http.StatusForbidden, denied.Code, "explicit-wins over the global chain-wins: %s", denied.Raw)

	setRelaunchPolicy(t, f, "group_env", "reuse")
	inherited := spawn("inherited-global", rows("group_env", "reuse"))
	assert.Equalf(t, http.StatusOK, inherited.Code, "reuse the global table already gives is free: %s", inherited.Raw)
	setRelaunchPolicy(t, f, "group_env", "re-resolve")

	rec := profileReq(t, f, http.MethodPost, "/v1/spawn-profiles", map[string]any{
		"name": "pinned", "relaunch_policy": map[string]string{"sandbox_profile_contents": "reuse"}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	viaProfile := spawn("via-profile", map[string]any{"profile": "pinned", "relaunch_policy": map[string]string{"sandbox_profile_contents": "reuse"}})
	assert.Equalf(t, http.StatusOK, viaProfile.Code, "reuse the profile already gives is free: %s", viaProfile.Raw)

	require.NoError(t, db.GrantAgentPermission(parent.ConvID, agentd.PermRelaunchPolicyManage, "test"))
	allowed := spawn("allowed", rows("group_env", "reuse"))
	assert.Equalf(t, http.StatusOK, allowed.Code, "with relaunch-policy.manage: %s", allowed.Raw)

	human := spawnWithRelaunch(t, f, f.AsHuman(), "human", rows("group_env", "reuse"))
	assert.Equalf(t, http.StatusOK, human.Code, "a human is not gated: %s", human.Raw)
}

// A profile cannot launder what a spawn's own row would need: an agent
// writing a keeps-old-settings row into a profile needs relaunch-policy.manage
// too, while a row the profile already carried stays free to keep.
func TestRelaunchOverrides_ProfileWriteGate(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	scribe := spawnWithRelaunch(t, f, f.AsHuman(), "scribe", nil)
	require.Equalf(t, http.StatusOK, scribe.Code, "spawn body=%s", scribe.Raw)
	require.NoError(t, db.GrantAgentPermission(scribe.ConvID, agentd.PermProfilesManage, "test"))
	write := func(method, path string, body map[string]any) int {
		return agentReq(t, f, scribe.ConvID, method, path, body).Code
	}

	assert.Equal(t, http.StatusForbidden, write(http.MethodPost, "/v1/spawn-profiles",
		map[string]any{"name": "agent-reuse", "relaunch_policy": map[string]string{"group_env": "reuse"}}))
	assert.Equal(t, http.StatusCreated, write(http.MethodPost, "/v1/spawn-profiles",
		map[string]any{"name": "agent-tighten", "relaunch_policy": map[string]string{"harness_config": "tighten-only"}}))

	rec := profileReq(t, f, http.MethodPost, "/v1/spawn-profiles", map[string]any{
		"name": "human-reuse", "relaunch_policy": map[string]string{"group_env": "reuse"}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Equal(t, http.StatusOK, write(http.MethodPatch, "/v1/spawn-profiles/human-reuse",
		map[string]any{"name": "human-reuse", "descr": "edited", "relaunch_policy": map[string]string{"group_env": "reuse"}}),
		"keeping a row the profile already carried is free")
	assert.Equal(t, http.StatusForbidden, write(http.MethodPatch, "/v1/spawn-profiles/human-reuse",
		map[string]any{"name": "human-reuse", "relaunch_policy": map[string]string{"group_env": "reuse", "harness_config": "explicit-wins"}}),
		"adding one is not")

	require.NoError(t, db.GrantAgentPermission(scribe.ConvID, agentd.PermRelaunchPolicyManage, "test"))
	assert.Equal(t, http.StatusCreated, write(http.MethodPost, "/v1/spawn-profiles",
		map[string]any{"name": "agent-reuse", "relaunch_policy": map[string]string{"group_env": "reuse"}}))
}

// A worker launched without the spawn dialog (a trigger, a template deploy)
// takes its profile tiers' relaunch rows as well.
func TestRelaunchOverrides_ManagedLaunchesTakeTheProfileRows(t *testing.T) {
	want := []db.RelaunchPolicyOverride{{Field: "group_env", Mode: "reuse", Source: "profile:reviewer"}}

	f := uniformTriggerGroup(t)
	_, err := db.CreateSpawnProfile(&db.SpawnProfile{Name: "reviewer", Harness: "claude", RelaunchPolicy: map[string]string{"group_env": "reuse"}})
	require.NoError(t, err)
	conv := fireUniformTrigger(t, f, "reviewer")
	assert.Equal(t, want, launchLayers(t, conv).RelaunchOverrides, "a trigger worker")

	f = newFlow(t)
	_, err = db.CreateSpawnProfile(&db.SpawnProfile{Name: "reviewer", Harness: "claude", RelaunchPolicy: map[string]string{"group_env": "reuse"}})
	require.NoError(t, err)
	conv = deployUniformTemplate(t, f, map[string]any{"spawn_profile": "reviewer"})
	assert.Equal(t, want, launchLayers(t, conv).RelaunchOverrides, "a template-deployed member")
}

// A keeps-old-settings row an agent sets only because it would inherit it
// anyway stays with its source, so a later tightening still reaches the
// child: Follow global for the global table, and the profile's own row.
func TestRelaunchOverrides_FreeInheritedRowStaysWithItsSource(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("crew")
	parent := spawnWithRelaunch(t, f, f.AsHuman(), "lead", nil)
	require.Equalf(t, http.StatusOK, parent.Code, "spawn body=%s", parent.Raw)
	require.NoError(t, db.GrantAgentPermission(parent.ConvID, agentd.PermGroupsMembersSpawn, "test"))
	rec := profileReq(t, f, http.MethodPost, "/v1/spawn-profiles", map[string]any{
		"name": "pinned", "relaunch_policy": map[string]string{"group_env": "reuse"}})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	child := spawnWithRelaunch(t, f, f.AsAgent(parent.ConvID), "child", map[string]any{
		"profile": "pinned", "relaunch_policy": map[string]string{"harness_config": "explicit-wins", "group_env": "reuse"}})
	require.Equalf(t, http.StatusOK, child.Code, "spawn body=%s", child.Raw)
	assert.ElementsMatch(t, []db.RelaunchPolicyOverride{
		{Field: "group_env", Mode: "reuse", Source: "profile:pinned"},
		{Field: "harness_config", Mode: "inherit", Source: "explicit"},
	}, launchLayers(t, child.ConvID).RelaunchOverrides)

	setRelaunchPolicy(t, f, "harness_config", "tighten-only")
	note := agentd.RelaunchOverridesNoteForTest(child.ConvID)
	assert.NotContains(t, note, "harness_config", "the global tightening reaches the child: %s", note)

	human := spawnWithRelaunch(t, f, f.AsHuman(), "human", map[string]any{"relaunch_policy": map[string]string{"harness_config": "explicit-wins"}})
	require.Equalf(t, http.StatusOK, human.Code, "spawn body=%s", human.Raw)
	assert.Equal(t, []db.RelaunchPolicyOverride{{Field: "harness_config", Mode: "explicit-wins", Source: "explicit"}},
		launchLayers(t, human.ConvID).RelaunchOverrides, "a human's row is a deliberate pin")
}

package agentd_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

const globalDefaultProfilePrefKey = "tclaude.dash.default_profile"

func setGlobalProfile(t *testing.T, f *testharness.Flow, profile string) *httptest.ResponseRecorder {
	t.Helper()
	r := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPut,
		"/v1/spawn-profile-default", map[string]any{"name": profile}))
	return testharness.Serve(f.Mux, r)
}

func spawnModel(t *testing.T, f *testharness.Flow, resp testharness.SpawnResp) string {
	t.Helper()
	require.Equalf(t, http.StatusOK, resp.Code, "spawn body=%s", resp.Raw)
	got, ok := f.World.SpawnModel(resp.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", resp.ConvID)
	return got
}

// The explicit request field is the highest tier: neither the group's profile
// nor the global profile may replace it.
func TestSpawnProfilePrecedence_ExplicitModelWins(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f,
		map[string]any{"name": "group", "harness": "codex", "model": "gpt-5.4"}).Code)
	require.Equal(t, http.StatusCreated, createProfile(t, f,
		map[string]any{"name": "global", "harness": "codex", "model": "gpt-5.5"}).Code)
	require.Equal(t, http.StatusOK, setGroupProfile(t, f, "alpha", "group").Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "global").Code)

	got := spawnModel(t, f, f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker", "harness": "codex", "model": "gpt-5.6-sol",
	}))
	assert.Equal(t, "gpt-5.6-sol", got)
}

func TestSpawnProfilePrecedence_ExplicitClaudeModelRejectedByCodexDefault(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f,
		map[string]any{"name": "global", "harness": "codex", "model": "gpt-5.6-sol"}).Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "global").Code)
	bridgeAgentClientToMux(t, f.Mux)
	chdirTo(t, resolveSym(t, t.TempDir()))

	stderr := new(bytes.Buffer)
	resp, rc := agent.RunSpawn(&agent.SpawnParams{
		Group: "alpha", Name: "worker", Model: "sonnet",
	}, new(bytes.Buffer), stderr, new(bytes.Buffer))
	assert.NotEqual(t, 0, rc)
	assert.Nil(t, resp)
	assert.Contains(t, stderr.String(), "not valid for codex")
}

func TestSpawnProfilePrecedence_ExplicitAPIModelRejectedByCodexDefault(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f,
		map[string]any{"name": "global", "harness": "codex", "model": "gpt-5.6-sol"}).Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "global").Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker", "model": "sonnet",
	})
	assert.Equal(t, http.StatusBadRequest, spawn.Code)
	assert.Contains(t, string(spawn.Raw), "invalid_model")
}

// An explicitly passed CLI --profile fills fields before the daemon considers
// the group and global profiles.
func TestSpawnProfilePrecedence_ExplicitProfileWins(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	for _, p := range []map[string]any{
		{"name": "explicit", "harness": "codex", "model": "gpt-5.6-sol"},
		{"name": "group", "harness": "codex", "model": "gpt-5.5"},
		{"name": "global", "harness": "codex", "model": "gpt-5.4"},
	} {
		require.Equal(t, http.StatusCreated, createProfile(t, f, p).Code)
	}
	require.Equal(t, http.StatusOK, setGroupProfile(t, f, "alpha", "group").Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "global").Code)
	bridgeAgentClientToMux(t, f.Mux)
	chdirTo(t, resolveSym(t, t.TempDir()))

	stderr := new(bytes.Buffer)
	resp, rc := agent.RunSpawn(&agent.SpawnParams{
		Group: "alpha", Name: "worker", Profile: "explicit",
	}, new(bytes.Buffer), stderr, new(bytes.Buffer))
	require.Equal(t, 0, rc, "RunSpawn stderr=%s", stderr.String())
	require.NotNil(t, resp)
	got, ok := f.World.SpawnModel(resp.ConvID)
	require.True(t, ok)
	assert.Equal(t, "gpt-5.6-sol", got)
}

func TestDisabledSpawnProfilesBlockEveryDefaultTier(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *testharness.Flow)
		body  map[string]any
	}{
		{
			name:  "explicit",
			setup: func(*testing.T, *testharness.Flow) {},
			body:  map[string]any{"name": "worker", "profile": "paused"},
		},
		{
			name: "group default",
			setup: func(t *testing.T, f *testharness.Flow) {
				require.Equal(t, http.StatusOK, setGroupProfile(t, f, "alpha", "paused").Code)
			},
			body: map[string]any{"name": "worker"},
		},
		{
			name: "global default",
			setup: func(t *testing.T, f *testharness.Flow) {
				require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "paused").Code)
			},
			body: map[string]any{"name": "worker"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlow(t)
			f.HaveGroup("alpha")
			require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
				"name": "paused", "disabled": true, "disabled_reason": "vendor outage until 18:00 UTC",
			}).Code)
			tc.setup(t, f)

			spawn := f.AsHuman().SpawnWith("alpha", tc.body)
			assert.Equal(t, http.StatusConflict, spawn.Code)
			assert.Contains(t, string(spawn.Raw), "profile_disabled")
			assert.Contains(t, string(spawn.Raw), `spawn profile \"paused\" is disabled: vendor outage until 18:00 UTC`)
		})
	}
}

func TestEnabledSpawnProfileWithRememberedReasonCanSpawn(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "restored", "disabled": false, "disabled_reason": "previous provider outage",
	}).Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "worker", "profile": "restored"})
	assert.Equal(t, http.StatusOK, spawn.Code, "remembered reason must not act as the disable switch")
}

func TestSpawnProfilePrecedence_ExplicitProfileAliasWinsAndIsDisclosed(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "gpt5.6-sol-high", "aliases": []string{"codex-reviewer"},
		"harness": "codex", "model": "gpt-5.6-sol", "effort": "high",
	}).Code)
	bridgeAgentClientToMux(t, f.Mux)
	chdirTo(t, resolveSym(t, t.TempDir()))

	stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
	resp, rc := agent.RunSpawn(&agent.SpawnParams{
		Group: "alpha", Name: "worker", Profile: "codex-reviewer",
	}, stdout, stderr, new(bytes.Buffer))
	require.Equal(t, 0, rc, "RunSpawn stderr=%s", stderr.String())
	require.NotNil(t, resp)
	assert.Contains(t, stdout.String(), `profile "gpt5.6-sol-high" via alias "codex-reviewer"`)
	got, ok := f.World.SpawnModel(resp.ConvID)
	require.True(t, ok)
	assert.Equal(t, "gpt-5.6-sol", got)
}

// A group profile outranks the global profile.
func TestSpawnProfilePrecedence_GroupDefaultWins(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f,
		map[string]any{"name": "group", "harness": "codex", "model": "gpt-5.6-terra"}).Code)
	require.Equal(t, http.StatusCreated, createProfile(t, f,
		map[string]any{"name": "global", "harness": "codex", "model": "gpt-5.5"}).Code)
	require.Equal(t, http.StatusOK, setGroupProfile(t, f, "alpha", "group").Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "global").Code)

	got := spawnModel(t, f, f.AsHuman().SpawnHarness("alpha", "worker", "codex"))
	assert.Equal(t, "gpt-5.6-terra", got)
}

// With no group profile, the dashboard/global default is now a real daemon
// tier rather than UI-only form prefill.
func TestSpawnProfilePrecedence_GlobalDefaultApplied(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f,
		map[string]any{"name": "global", "harness": "codex", "model": "gpt-5.6-sol"}).Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "global").Code)
	bridgeAgentClientToMux(t, f.Mux)
	chdirTo(t, resolveSym(t, t.TempDir()))

	stderr := new(bytes.Buffer)
	resp, rc := agent.RunSpawn(&agent.SpawnParams{Group: "alpha", Name: "worker"},
		new(bytes.Buffer), stderr, new(bytes.Buffer))
	require.Equal(t, 0, rc, "RunSpawn stderr=%s", stderr.String())
	require.NotNil(t, resp)
	got, ok := f.World.SpawnModel(resp.ConvID)
	require.True(t, ok)
	assert.Equal(t, "gpt-5.6-sol", got)
}

// With no value at any tier the model remains empty at the production Spawner
// boundary. model_spawn_test.go pins the resulting Codex argv: no --model.
func TestSpawnProfilePrecedence_NothingLeavesModelUnset(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	got := spawnModel(t, f, f.AsHuman().SpawnHarness("alpha", "worker", "codex"))
	assert.Empty(t, got)
}

// Resolution is per field, not "first profile wins": the group contributes
// effort while its blank model falls through to the compatible global profile.
func TestSpawnProfilePrecedence_PerFieldFallsThrough(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f,
		map[string]any{"name": "group", "harness": "codex", "effort": "high"}).Code)
	require.Equal(t, http.StatusCreated, createProfile(t, f,
		map[string]any{"name": "global", "harness": "codex", "model": "gpt-5.6-luna", "effort": "low"}).Code)
	require.Equal(t, http.StatusOK, setGroupProfile(t, f, "alpha", "group").Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "global").Code)

	spawn := f.AsHuman().SpawnHarness("alpha", "worker", "codex")
	assert.Equal(t, "gpt-5.6-luna", spawnModel(t, f, spawn))
	effort, ok := f.World.SpawnEffort(spawn.ConvID)
	require.True(t, ok)
	assert.Equal(t, "high", effort)
}

func TestSpawnProfilePrecedence_SparseExplicitProfileFallsThrough(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f,
		map[string]any{"name": "explicit", "harness": "codex", "effort": "high"}).Code)
	require.Equal(t, http.StatusCreated, createProfile(t, f,
		map[string]any{"name": "group", "harness": "codex", "model": "gpt-5.6-sol", "effort": "low"}).Code)
	require.Equal(t, http.StatusOK, setGroupProfile(t, f, "alpha", "group").Code)
	bridgeAgentClientToMux(t, f.Mux)
	chdirTo(t, resolveSym(t, t.TempDir()))

	stderr := new(bytes.Buffer)
	resp, rc := agent.RunSpawn(&agent.SpawnParams{
		Group: "alpha", Name: "worker", Profile: "explicit",
	}, new(bytes.Buffer), stderr, new(bytes.Buffer))
	require.Equal(t, 0, rc, "RunSpawn stderr=%s", stderr.String())
	require.NotNil(t, resp)
	model, ok := f.World.SpawnModel(resp.ConvID)
	require.True(t, ok)
	assert.Equal(t, "gpt-5.6-sol", model, "blank explicit-profile model falls through to group")
	effort, ok := f.World.SpawnEffort(resp.ConvID)
	require.True(t, ok)
	assert.Equal(t, "high", effort, "explicit-profile effort still wins")
}

func TestSpawnProfilePrecedence_GroupFalseBoolDefaultsBlockGlobalTrue(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "group", "harness": "codex", "auto_review": false, "trust_dir": false,
	}).Code)
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "global", "harness": "codex", "auto_review": true, "trust_dir": true,
	}).Code)
	require.Equal(t, http.StatusOK, setGroupProfile(t, f, "alpha", "group").Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "global").Code)

	spawn := f.AsHuman().SpawnHarness("alpha", "worker", "codex")
	autoReview, ok := f.World.SpawnAutoReview(spawn.ConvID)
	require.True(t, ok)
	assert.False(t, autoReview, "group's explicit false blocks global true")
	trustDir, ok := f.World.SpawnTrustDir(spawn.ConvID)
	require.True(t, ok)
	assert.False(t, trustDir, "group's explicit false blocks global true")
}

func TestSpawnProfilePrecedence_ExplicitAPIFalseBoolsBlockGlobalTrue(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "global", "harness": "codex", "auto_review": true, "trust_dir": true,
	}).Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "global").Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker", "harness": "codex", "auto_review": false, "trust_dir": false,
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	autoReview, ok := f.World.SpawnAutoReview(spawn.ConvID)
	require.True(t, ok)
	assert.False(t, autoReview)
	trustDir, ok := f.World.SpawnTrustDir(spawn.ConvID)
	require.True(t, ok)
	assert.False(t, trustDir)
}

func TestSpawnProfilePrecedence_ExplicitCLIProfileFalseBoolsBlockLowerTrue(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "explicit", "harness": "codex", "auto_review": false, "trust_dir": false,
	}).Code)
	require.Equal(t, http.StatusCreated, createProfile(t, f, map[string]any{
		"name": "global", "harness": "codex", "auto_review": true, "trust_dir": true,
	}).Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "global").Code)
	bridgeAgentClientToMux(t, f.Mux)
	chdirTo(t, resolveSym(t, t.TempDir()))

	stderr := new(bytes.Buffer)
	resp, rc := agent.RunSpawn(&agent.SpawnParams{
		Group: "alpha", Name: "worker", Profile: "explicit",
	}, new(bytes.Buffer), stderr, new(bytes.Buffer))
	require.Equal(t, 0, rc, "RunSpawn stderr=%s", stderr.String())
	require.NotNil(t, resp)
	autoReview, ok := f.World.SpawnAutoReview(resp.ConvID)
	require.True(t, ok)
	assert.False(t, autoReview)
	trustDir, ok := f.World.SpawnTrustDir(resp.ConvID)
	require.True(t, ok)
	assert.False(t, trustDir)
}

func TestSpawnProfilePrecedence_RemoteControlRejectedByCodexDefault(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f,
		map[string]any{"name": "global", "harness": "codex", "model": "gpt-5.6-sol"}).Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "global").Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker", "remote_control": true,
	})
	require.Equalf(t, http.StatusBadRequest, spawn.Code, "spawn body=%s", spawn.Raw)

	bridgeAgentClientToMux(t, f.Mux)
	chdirTo(t, resolveSym(t, t.TempDir()))
	stderr := new(bytes.Buffer)
	resp, rc := agent.RunSpawn(&agent.SpawnParams{
		Group: "alpha", Name: "cli-worker", RemoteControl: true,
	}, new(bytes.Buffer), stderr, new(bytes.Buffer))
	assert.NotEqual(t, 0, rc)
	assert.Nil(t, resp)
	assert.Contains(t, stderr.String(), "no built-in remote access")
}

// A global preference may become stale after profile deletion. Spawn degrades
// to the next tier instead of failing; setting an already-missing profile is
// rejected eagerly by the API.
func TestSpawnProfilePrecedence_StaleGlobalDefaultSkipped(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.NoError(t, db.SetDashboardPref(globalDefaultProfilePrefKey, "deleted-profile"))

	got := spawnModel(t, f, f.AsHuman().SpawnHarness("alpha", "worker", "codex"))
	assert.Empty(t, got)

	rec := setGlobalProfile(t, f, "also-missing")
	assert.Equalf(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
}

func TestGlobalDefaultSpawnProfileAPI_ShowAndClear(t *testing.T) {
	f := newFlow(t)
	require.Equal(t, http.StatusCreated, createProfile(t, f,
		map[string]any{"name": "global", "harness": "codex", "model": "gpt-5.6-sol"}).Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "global").Code)

	show := testharness.Serve(f.Mux, agentd.AsHumanPeer(
		testharness.JSONRequest(t, http.MethodGet, "/v1/spawn-profile-default", nil)))
	require.Equal(t, http.StatusOK, show.Code)
	assert.JSONEq(t, `{"name":"global"}`, show.Body.String())

	clear := testharness.Serve(f.Mux, agentd.AsHumanPeer(
		testharness.JSONRequest(t, http.MethodDelete, "/v1/spawn-profile-default", nil)))
	require.Equal(t, http.StatusOK, clear.Code)
	_, present, err := db.GetDashboardPref(globalDefaultProfilePrefKey)
	require.NoError(t, err)
	assert.False(t, present)
}

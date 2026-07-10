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

func TestSpawnProfilePrecedence_ExplicitClaudeModelPinsDefaultHarness(t *testing.T) {
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
	require.Equal(t, 0, rc, "RunSpawn stderr=%s", stderr.String())
	require.NotNil(t, resp)
	model, ok := f.World.SpawnModel(resp.ConvID)
	require.True(t, ok)
	assert.Equal(t, "sonnet", model)
}

func TestSpawnProfilePrecedence_ExplicitAPIModelPinsDefaultHarness(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")
	require.Equal(t, http.StatusCreated, createProfile(t, f,
		map[string]any{"name": "global", "harness": "codex", "model": "gpt-5.6-sol"}).Code)
	require.Equal(t, http.StatusOK, setGlobalProfile(t, f, "global").Code)

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker", "model": "sonnet",
	})
	assert.Equal(t, "sonnet", spawnModel(t, f, spawn))
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

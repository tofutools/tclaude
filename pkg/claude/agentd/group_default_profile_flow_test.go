package agentd_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// These flow tests cover the JOH-210 inc2 spawn-resolution path: a group's
// default spawn profile fills blank launch fields server-side, replacing the
// retired Claude-only default_model. We assert at the Spawner boundary (the
// production seam where the --model/--harness/… flags are later built), the
// same surface spawn_model_flow_test.go uses.

// createProfile POSTs a spawn profile through the daemon (the human peer
// bypasses the profiles.manage gate). Returns the recorder for status/body
// assertions.
func createProfile(t *testing.T, f *testharness.Flow, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	r := agentd.AsHumanPeer(testharness.JSONRequest(t, http.MethodPost, "/v1/spawn-profiles", body))
	return testharness.Serve(f.Mux, r)
}

// setGroupProfile PATCHes a group's default_profile reference.
func setGroupProfile(t *testing.T, f *testharness.Flow, group, profile string) *httptest.ResponseRecorder {
	t.Helper()
	r := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPatch, "/v1/groups/"+group,
		map[string]any{"default_profile": profile}))
	return testharness.Serve(f.Mux, r)
}

// Scenario: a group's default profile carries a model. A blank-model spawn
// into that group inherits it — the core promise that replaces the old
// per-group default_model.
func TestGroupDefaultProfile_AppliedToSpawn(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	rec := createProfile(t, f, map[string]any{"name": "team-default", "model": "sonnet"})
	require.Equalf(t, http.StatusCreated, rec.Code, "create profile body=%s", rec.Body.String())

	rec = setGroupProfile(t, f, "alpha", "team-default")
	require.Equalf(t, http.StatusOK, rec.Code, "set default_profile body=%s", rec.Body.String())

	// It landed on the group row — the surface GetAgentGroupByName (and the
	// dashboard snapshot) reads.
	g, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Equal(t, "team-default", g.DefaultProfile, "group row default_profile")

	spawn := f.AsHuman().Spawn("alpha", "worker")
	got, ok := f.World.SpawnModel(spawn.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", spawn.ConvID)
	assert.Equal(t, "sonnet", got, "blank-model spawn must inherit the profile's model")
}

// Scenario: an explicit model in the spawn request wins over the group's
// default profile. The profile only fills a BLANK field.
func TestGroupDefaultProfile_ExplicitModelOverrides(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	require.Equalf(t, http.StatusCreated,
		createProfile(t, f, map[string]any{"name": "p-opus", "model": "opus"}).Code, "create profile")
	require.Equalf(t, http.StatusOK, setGroupProfile(t, f, "alpha", "p-opus").Code, "set default_profile")

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":  "worker",
		"model": "fable[1m]",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	got, ok := f.World.SpawnModel(spawn.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", spawn.ConvID)
	assert.Equal(t, "fable[1m]", got, "explicit model must override the profile's model")
}

// Scenario: clearing the default profile (PATCH default_profile:"") removes
// it. A subsequent blank-model spawn threads "" through again, so the
// production spawner omits --model and the harness resolves its own default.
func TestGroupDefaultProfile_PatchClears(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	require.Equalf(t, http.StatusCreated,
		createProfile(t, f, map[string]any{"name": "p-opus", "model": "opus"}).Code, "create profile")
	require.Equalf(t, http.StatusOK, setGroupProfile(t, f, "alpha", "p-opus").Code, "set default_profile")

	rec := setGroupProfile(t, f, "alpha", "")
	require.Equalf(t, http.StatusOK, rec.Code, "clear default_profile body=%s", rec.Body.String())

	g, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Empty(t, g.DefaultProfile, "default_profile should be cleared")

	spawn := f.AsHuman().Spawn("alpha", "worker")
	got, ok := f.World.SpawnModel(spawn.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", spawn.ConvID)
	assert.Equal(t, "", got, `cleared default must thread "" (production omits --model)`)
}

// Scenario: pointing a group at a profile that does not exist is a 400 and
// nothing is stored — there is no DB-level foreign key, so the handler does the
// referential check.
func TestGroupDefaultProfile_RejectsUnknownProfile(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	rec := setGroupProfile(t, f, "alpha", "ghost")
	assert.Equalf(t, http.StatusBadRequest, rec.Code,
		"unknown profile reference should 400; body=%s", rec.Body.String())

	g, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Empty(t, g.DefaultProfile, "rejected reference must not be stored")
}

// Scenario: the harness-correct fill (#343). A Codex profile carries a Codex
// model + effort + sandbox; a blank spawn into the group resolves the WHOLE
// launch shape from the profile — harness, model, effort and sandbox — so the
// spawned session is a Codex session running the profile's model. This is the
// case the old default_model could not express (its model was validated
// Claude-only and forwarded without a harness).
func TestGroupDefaultProfile_HarnessCorrectFill(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	rec := createProfile(t, f, map[string]any{
		"name":    "cx",
		"harness": "codex",
		"model":   "gpt-5-codex",
		"effort":  "high",
		"sandbox": "read-only",
	})
	require.Equalf(t, http.StatusCreated, rec.Code, "create codex profile body=%s", rec.Body.String())
	require.Equalf(t, http.StatusOK, setGroupProfile(t, f, "alpha", "cx").Code, "set default_profile")

	spawn := f.AsHuman().Spawn("alpha", "worker")

	model, ok := f.World.SpawnModel(spawn.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", spawn.ConvID)
	assert.Equal(t, "gpt-5-codex", model, "profile's codex model fills the spawn")

	effort, _ := f.World.SpawnEffort(spawn.ConvID)
	assert.Equal(t, "high", effort, "profile's effort fills the spawn")

	sandbox, _ := f.World.SpawnSandbox(spawn.ConvID)
	assert.Equal(t, "read-only", sandbox, "profile's sandbox fills the spawn")

	// Harness filled through: the spawned session row is a codex session.
	rows, err := db.FindSessionsByConvID(spawn.ConvID)
	require.NoError(t, err)
	require.NotEmpty(t, rows, "spawned session row")
	assert.Equal(t, "codex", rows[0].Harness, "profile's harness drives the spawn")
}

// Scenario: a Codex default profile that leaves sandbox/approval BLANK must
// still launch the agent under the harness's secure defaults (workspace-write /
// never), not unsandboxed. This guards against the profile fill bypassing the
// secure-default resolution.
func TestGroupDefaultProfile_BlankCodexSandboxGetsSecureDefault(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	rec := createProfile(t, f, map[string]any{"name": "cx-bare", "harness": "codex", "model": "gpt-5-codex"})
	require.Equalf(t, http.StatusCreated, rec.Code, "create codex profile body=%s", rec.Body.String())
	require.Equalf(t, http.StatusOK, setGroupProfile(t, f, "alpha", "cx-bare").Code, "set default_profile")

	spawn := f.AsHuman().Spawn("alpha", "worker")

	sandbox, ok := f.World.SpawnSandbox(spawn.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", spawn.ConvID)
	assert.Equal(t, "workspace-write", sandbox, "blank profile sandbox must resolve to the Codex secure default, not unsandboxed")
	approval, _ := f.World.SpawnApproval(spawn.ConvID)
	assert.Equal(t, "never", approval, "blank profile approval must resolve to the Codex non-escalating default")
}

// Scenario: a spawn request that pins a DIFFERENT harness than the group's
// default profile does NOT inherit the profile's harness-specific fields — the
// profile is skipped, and the spawn runs on the pinned harness with its own
// defaults (no confusing cross-harness 400, no Codex model on a Claude spawn).
func TestGroupDefaultProfile_PinnedDifferentHarnessSkipsProfile(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	rec := createProfile(t, f, map[string]any{"name": "cx", "harness": "codex", "model": "gpt-5-codex"})
	require.Equalf(t, http.StatusCreated, rec.Code, "create codex profile body=%s", rec.Body.String())
	require.Equalf(t, http.StatusOK, setGroupProfile(t, f, "alpha", "cx").Code, "set default_profile")

	// Pin claude explicitly: the codex default profile must be ignored.
	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{"name": "worker", "harness": "claude"})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	model, ok := f.World.SpawnModel(spawn.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", spawn.ConvID)
	assert.Equal(t, "", model, "a claude-pinned spawn must not inherit the codex profile's model")

	rows, err := db.FindSessionsByConvID(spawn.ConvID)
	require.NoError(t, err)
	require.NotEmpty(t, rows, "spawned session row")
	assert.Equal(t, "claude", rows[0].Harness, "the pinned harness wins; the codex profile is skipped")
}

// Scenario: a group created with a default profile in one shot — POST
// /v1/groups carries default_profile, applied as a post-create update (the same
// pattern as default_cwd / default_context). A blank-model spawn inherits it.
func TestGroupDefaultProfile_CreateWithProfile(t *testing.T) {
	f := newFlow(t)

	require.Equalf(t, http.StatusCreated,
		createProfile(t, f, map[string]any{"name": "p-haiku", "model": "haiku"}).Code, "create profile")

	r := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/groups",
		map[string]any{"name": "beta", "default_profile": "p-haiku"}))
	rec := testharness.Serve(f.Mux, r)
	require.Equalf(t, http.StatusCreated, rec.Code, "create group body=%s", rec.Body.String())

	g, err := db.GetAgentGroupByName("beta")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Equal(t, "p-haiku", g.DefaultProfile, "created group carries the default profile")

	spawn := f.AsHuman().Spawn("beta", "worker")
	got, ok := f.World.SpawnModel(spawn.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", spawn.ConvID)
	assert.Equal(t, "haiku", got, "spawn inherited the create-time default profile's model")

	// Create referencing a non-existent profile is a 400 before the insert —
	// no group left behind.
	r = agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/groups",
		map[string]any{"name": "gamma", "default_profile": "ghost"}))
	rec = testharness.Serve(f.Mux, r)
	assert.Equalf(t, http.StatusBadRequest, rec.Code,
		"unknown profile reference should 400; body=%s", rec.Body.String())
	g, err = db.GetAgentGroupByName("gamma")
	require.NoError(t, err)
	assert.Nil(t, g, "rejected create must not leave a group behind")
}

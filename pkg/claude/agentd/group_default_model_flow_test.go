package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: a group has a default model. An agent spawned into that
// group with NO explicit model inherits it.
//
// This is the "group default model" feature's core promise: PATCH
// /v1/groups/{name} stores default_model, and executeSpawn substitutes
// it when the spawn request leaves model blank — so the default
// reaches every spawn path (CLI, API, dashboard, template
// instantiation), not just the dashboard's Default-option label. We
// assert at the Spawner boundary (the production seam where the
// --model flag is later built), exactly like the explicit-model suite
// in spawn_model_flow_test.go.
func TestGroupDefaultModel_AppliedToSpawn(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	// Set the group's default model via PATCH /v1/groups/alpha.
	r := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPatch, "/v1/groups/alpha",
		map[string]any{"default_model": "sonnet"}))
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "PATCH body=%s", rec.Body.String())

	// It landed on the group row — the surface GetAgentGroupByName
	// (and therefore the dashboard snapshot) reads.
	g, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Equal(t, "sonnet", g.DefaultModel, "group row default_model")

	// Spawn a worker with no explicit model: the group default must
	// reach the spawner.
	spawn := f.AsHuman().Spawn("alpha", "worker")
	got, ok := f.World.SpawnModel(spawn.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", spawn.ConvID)
	assert.Equal(t, "sonnet", got, "blank-model spawn must inherit the group default")
}

// Scenario: an explicit model in the spawn request wins over the
// group default. The default is only a fallback for a BLANK model — a
// caller that pins one must always get exactly that.
func TestGroupDefaultModel_ExplicitModelOverrides(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	if _, err := db.SetAgentGroupDefaultModel("alpha", "opus"); err != nil {
		t.Fatalf("SetAgentGroupDefaultModel: %v", err)
	}

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":  "worker",
		"model": "fable[1m]",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	got, ok := f.World.SpawnModel(spawn.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", spawn.ConvID)
	assert.Equal(t, "fable[1m]", got, "explicit model must override the group default")
}

// Scenario: clearing the default (PATCH default_model:"") removes it.
// A subsequent blank-model spawn threads "" through again, so the
// production spawner omits --model and claude resolves its own
// default.
func TestGroupDefaultModel_PatchClears(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	if _, err := db.SetAgentGroupDefaultModel("alpha", "opus"); err != nil {
		t.Fatalf("SetAgentGroupDefaultModel: %v", err)
	}

	r := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPatch, "/v1/groups/alpha",
		map[string]any{"default_model": ""}))
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "PATCH body=%s", rec.Body.String())

	g, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Empty(t, g.DefaultModel, "default_model should be cleared")

	spawn := f.AsHuman().Spawn("alpha", "worker")
	got, ok := f.World.SpawnModel(spawn.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", spawn.ConvID)
	assert.Equal(t, "", got, `cleared default must thread "" (production omits --model)`)
}

// Scenario: an invalid model alias in the PATCH is a 400 and nothing
// is stored — the same ValidateModel gate every other model surface
// uses. A full model ID, on the other hand, is explicitly allowed
// (claude --model accepts "a model's full name"), so a brand-new model
// is usable before tclaude's alias list catches up.
func TestGroupDefaultModel_PatchValidation(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	// Unknown alias → 400, not stored.
	r := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPatch, "/v1/groups/alpha",
		map[string]any{"default_model": "gpt"}))
	rec := testharness.Serve(f.Mux, r)
	assert.Equalf(t, http.StatusBadRequest, rec.Code,
		"invalid default_model should 400; body=%s", rec.Body.String())
	g, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Empty(t, g.DefaultModel, "rejected value must not be stored")

	// Full model ID (with the [1m] context-window suffix) → accepted,
	// stored normalised, and threads through to a blank-model spawn.
	r = agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPatch, "/v1/groups/alpha",
		map[string]any{"default_model": "Claude-Fable-5[1M]"}))
	rec = testharness.Serve(f.Mux, r)
	require.Equalf(t, http.StatusOK, rec.Code,
		"full model ID should be accepted; body=%s", rec.Body.String())
	g, err = db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Equal(t, "claude-fable-5[1m]", g.DefaultModel, "stored lower-cased")

	spawn := f.AsHuman().Spawn("alpha", "worker")
	got, ok := f.World.SpawnModel(spawn.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", spawn.ConvID)
	assert.Equal(t, "claude-fable-5[1m]", got, "full-ID default reaches the spawner")
}

// Scenario: a group created with a default model in one shot — POST
// /v1/groups carries default_model, applied as a post-create update
// (the same pattern as default_cwd / default_context). And the
// invalid-alias twin: rejected up front, before the insert, so a bad
// value never leaves a half-configured group behind.
func TestGroupDefaultModel_CreateWithModel(t *testing.T) {
	f := newFlow(t)

	r := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/groups",
		map[string]any{"name": "beta", "default_model": "haiku"}))
	rec := testharness.Serve(f.Mux, r)
	require.Equalf(t, http.StatusCreated, rec.Code, "create body=%s", rec.Body.String())

	g, err := db.GetAgentGroupByName("beta")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Equal(t, "haiku", g.DefaultModel, "created group carries the default model")

	// End-to-end: a blank-model spawn into the freshly created group
	// inherits the create-time default.
	spawn := f.AsHuman().Spawn("beta", "worker")
	got, ok := f.World.SpawnModel(spawn.ConvID)
	require.True(t, ok, "no spawn recorded for conv %s", spawn.ConvID)
	assert.Equal(t, "haiku", got, "spawn inherited the create-time default model")

	// Invalid alias on create: 400 before the insert — no group.
	r = agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/groups",
		map[string]any{"name": "gamma", "default_model": "gpt"}))
	rec = testharness.Serve(f.Mux, r)
	assert.Equalf(t, http.StatusBadRequest, rec.Code,
		"invalid default_model should 400; body=%s", rec.Body.String())
	g, err = db.GetAgentGroupByName("gamma")
	require.NoError(t, err)
	assert.Nil(t, g, "rejected create must not leave a group behind")
}

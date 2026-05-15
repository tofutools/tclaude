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

// Scenario: a group has a default start dir. An agent spawned into
// that group with NO explicit cwd inherits the group's default.
//
// This is the "group default start dir" feature's core promise:
// PATCH /v1/groups/{name} stores default_cwd, and handleGroupSpawn
// substitutes it server-side when the spawn request leaves cwd
// blank — so the default reaches the CLI and API too, not just the
// dashboard's client-side prefill. The Spawn DSL helper sends only
// `{alias}` (no cwd), which is exactly the blank-cwd path.
func TestGroupDefaultCwd_PrefillsSpawn(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	// A real directory — handleGroupSpawn runs the cwd through
	// resolveSpawnCwd, which rejects a path that doesn't exist.
	defaultDir := t.TempDir()

	// Set the group's default start dir via PATCH /v1/groups/alpha.
	r := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPatch, "/v1/groups/alpha",
		map[string]any{"default_cwd": defaultDir}))
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "PATCH body=%s", rec.Body.String())

	// It landed on the group row — the surface GetAgentGroupByName
	// (and therefore the dashboard snapshot) reads.
	g, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Equal(t, defaultDir, g.DefaultCwd, "group row default_cwd")

	// Spawn a worker with no explicit cwd.
	spawn := f.AsHuman().Spawn("alpha", "worker")

	// The spawned session runs in the group's default dir — what
	// `tclaude session ls` / the dashboard show as its cwd.
	s, err := db.LoadSession(spawn.Label)
	require.NoError(t, err)
	require.NotNil(t, s, "spawned session row missing")
	assert.Equal(t, defaultDir, s.Cwd, "spawned session inherited the group default dir")
}

// Scenario: an explicit cwd in the spawn request wins over the
// group default. The group default is only a fallback for a BLANK
// cwd — a caller that pins a dir must always get exactly that dir.
func TestGroupDefaultCwd_ExplicitCwdOverrides(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	// The group default is deliberately a path that does NOT exist:
	// this spawn pins an explicit cwd, so the default is never read
	// and never reaches resolveSpawnCwd's existence check.
	if _, err := db.SetAgentGroupDefaultCwd("alpha", "/work/alpha-team"); err != nil {
		t.Fatalf("SetAgentGroupDefaultCwd: %v", err)
	}

	// The explicit cwd is real — resolveSpawnCwd validates it exists.
	explicitDir := t.TempDir()
	r := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPost, "/v1/groups/alpha/spawn",
		map[string]any{"alias": "worker", "cwd": explicitDir}))
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "spawn body=%s", rec.Body.String())

	var spawn testharness.SpawnResp
	testharness.DecodeJSON(t, rec, &spawn)
	require.NotEmpty(t, spawn.Label, "spawn response missing label")

	s, err := db.LoadSession(spawn.Label)
	require.NoError(t, err)
	require.NotNil(t, s, "spawned session row missing")
	assert.Equal(t, explicitDir, s.Cwd, "explicit cwd must override the group default")
}

// Scenario: clearing the default (PATCH default_cwd:"") removes it.
// A subsequent blank-cwd spawn no longer picks up the old value.
func TestGroupDefaultCwd_PatchClears(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	if _, err := db.SetAgentGroupDefaultCwd("alpha", "/work/alpha-team"); err != nil {
		t.Fatalf("SetAgentGroupDefaultCwd: %v", err)
	}

	// Clear it back out via the PATCH endpoint.
	r := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPatch, "/v1/groups/alpha",
		map[string]any{"default_cwd": ""}))
	rec := testharness.Serve(f.Mux, r)
	require.Equal(t, http.StatusOK, rec.Code, "PATCH body=%s", rec.Body.String())

	g, err := db.GetAgentGroupByName("alpha")
	require.NoError(t, err)
	require.NotNil(t, g)
	assert.Empty(t, g.DefaultCwd, "default_cwd should be cleared")
}

// Scenario: PATCH /v1/groups/{name} with an empty body is a 400 —
// the partial-update contract needs at least one field to act on.
func TestGroupDefaultCwd_PatchEmptyBodyRejected(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	r := agentd.AsHumanPeer(testharness.JSONRequest(t,
		http.MethodPatch, "/v1/groups/alpha", map[string]any{}))
	rec := testharness.Serve(f.Mux, r)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "empty PATCH body=%s", rec.Body.String())
}

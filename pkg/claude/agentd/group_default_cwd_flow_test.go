package agentd_test

import (
	"net/http"
	"testing"
	"testing/synctest"

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
// `{name}` (no cwd), which is exactly the blank-cwd path.
func TestGroupDefaultCwd_PrefillsSpawn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
	})
}

// Scenario: an explicit cwd in the spawn request wins over the
// group default. The group default is only a fallback for a BLANK
// cwd — a caller that pins a dir must always get exactly that dir.
func TestGroupDefaultCwd_ExplicitCwdOverrides(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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
		spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
			"name": "worker",
			"cwd":  explicitDir,
		})
		require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
		require.NotEmpty(t, spawn.Label, "spawn response missing label")

		s, err := db.LoadSession(spawn.Label)
		require.NoError(t, err)
		require.NotNil(t, s, "spawned session row missing")
		assert.Equal(t, explicitDir, s.Cwd, "explicit cwd must override the group default")
	})
}

// Scenario: clearing the default (PATCH default_cwd:"") removes it.
// A subsequent blank-cwd spawn no longer picks up the old value.
func TestGroupDefaultCwd_PatchClears(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
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

		// End-to-end: a later blank-cwd spawn must NOT inherit the
		// now-cleared default — the behavioural half of the contract.
		spawn := f.AsHuman().Spawn("alpha", "worker")
		s, err := db.LoadSession(spawn.Label)
		require.NoError(t, err)
		require.NotNil(t, s, "spawned session row missing")
		assert.NotEqual(t, "/work/alpha-team", s.Cwd, "cleared default must not be used")
	})
}

// Scenario: a relative default_cwd is rejected. A relative path would
// resolve against the daemon's own cwd at spawn time — meaningless —
// so handleGroupUpdate (via resolveGroupDefaultCwd) 400s it rather
// than silently storing it.
func TestGroupDefaultCwd_PatchRejectsRelative(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)
		f.HaveGroup("alpha")

		r := agentd.AsHumanPeer(testharness.JSONRequest(t,
			http.MethodPatch, "/v1/groups/alpha",
			map[string]any{"default_cwd": "relative/sub/dir"}))
		rec := testharness.Serve(f.Mux, r)
		assert.Equalf(t, http.StatusBadRequest, rec.Code,
			"relative default_cwd should 400; body=%s", rec.Body.String())

		// And nothing was persisted.
		g, err := db.GetAgentGroupByName("alpha")
		require.NoError(t, err)
		require.NotNil(t, g)
		assert.Empty(t, g.DefaultCwd, "rejected value must not be stored")
	})
}

// Scenario: PATCH /v1/groups/{name} with an empty body is a 400 —
// the partial-update contract needs at least one field to act on.
func TestGroupDefaultCwd_PatchEmptyBodyRejected(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)
		f.HaveGroup("alpha")

		r := agentd.AsHumanPeer(testharness.JSONRequest(t,
			http.MethodPatch, "/v1/groups/alpha", map[string]any{}))
		rec := testharness.Serve(f.Mux, r)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "empty PATCH body=%s", rec.Body.String())
	})
}

// Scenario: a group created with a default start dir in one shot —
// POST /v1/groups carries default_cwd, applied as a post-create
// update. This is the create-time path the dashboard's create modal
// rides: the "Default cwd" field is sent alongside name/descr/context.
func TestGroupDefaultCwd_CreateWithCwd(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		defaultDir := t.TempDir()
		r := agentd.AsHumanPeer(testharness.JSONRequest(t,
			http.MethodPost, "/v1/groups",
			map[string]any{"name": "beta", "default_cwd": defaultDir}))
		rec := testharness.Serve(f.Mux, r)
		require.Equalf(t, http.StatusCreated, rec.Code, "create body=%s", rec.Body.String())

		g, err := db.GetAgentGroupByName("beta")
		require.NoError(t, err)
		require.NotNil(t, g)
		assert.Equal(t, defaultDir, g.DefaultCwd, "created group carries the default cwd")

		// End-to-end: a blank-cwd spawn into the freshly created group
		// inherits the default set at create time.
		spawn := f.AsHuman().Spawn("beta", "worker")
		s, err := db.LoadSession(spawn.Label)
		require.NoError(t, err)
		require.NotNil(t, s, "spawned session row missing")
		assert.Equal(t, defaultDir, s.Cwd, "spawn inherited the create-time default dir")
	})
}

// Scenario: a relative default_cwd in the create payload is rejected
// up front — before the group is inserted — so a bad value never
// leaves a group behind. Mirrors the PATCH-path rejection.
func TestGroupDefaultCwd_CreateRejectsRelative(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFlow(t)

		r := agentd.AsHumanPeer(testharness.JSONRequest(t,
			http.MethodPost, "/v1/groups",
			map[string]any{"name": "beta", "default_cwd": "relative/sub/dir"}))
		rec := testharness.Serve(f.Mux, r)
		assert.Equalf(t, http.StatusBadRequest, rec.Code,
			"relative default_cwd should 400; body=%s", rec.Body.String())

		// The create was rejected before the insert — no group exists.
		g, err := db.GetAgentGroupByName("beta")
		require.NoError(t, err)
		assert.Nil(t, g, "rejected create must not leave a group behind")
	})
}

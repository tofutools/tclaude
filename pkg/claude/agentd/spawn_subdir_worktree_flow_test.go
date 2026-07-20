package agentd_test

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario: a human spawns an agent whose launch dir (cwd) is a
// "virtual monorepo" — a plain folder of shared docs — while the
// actual code work belongs in a git worktree of a nested sub-repo.
//
// The dashboard creates the worktree first, then spawns with cwd =
// the monorepo plus worktree_path / worktree_branch. The daemon must:
//   - launch the agent in the monorepo (NOT the worktree), so the
//     top-level CLAUDE.md / docs are its working context;
//   - surface the worktree path + branch in the welcome message so
//     the agent knows where to make its code edits.
func TestSpawn_SubdirWorktree_LaunchesInMonorepoTellsWorktree(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	// Two real dirs: the monorepo launch dir and the worktree the
	// dashboard would have created under it. resolveSpawnCwd
	// stat-checks both, so they must exist.
	monorepo := t.TempDir()
	worktreeDir := t.TempDir()

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":            "worker",
		"cwd":             monorepo,
		"worktree_path":   worktreeDir,
		"worktree_branch": "feature-x",
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)
	require.NotEmpty(t, spawn.Label, "spawn response missing label")

	// The agent launched in the monorepo, not the worktree — that's
	// the whole promise of the sub-repo-worktree flow.
	s, err := db.LoadSession(spawn.Label)
	require.NoError(t, err)
	require.NotNil(t, s, "spawned session row missing")
	assert.Equal(t, monorepo, s.Cwd,
		"agent should launch in the monorepo (cwd), not the worktree")

	// The welcome (delivered as the launch prompt) names the worktree
	// path + branch so the agent edits code in the right place.
	f.AssertSpawnInitialPrompt(spawn.ConvID, worktreeDir, 10*time.Second)
	f.AssertSpawnInitialPrompt(spawn.ConvID, "feature-x", 10*time.Second)
}

// Scenario: an invalid worktree_path (a typo, a stale dir) is caught
// up front with a 400 — the same treatment a bad cwd already gets —
// rather than producing a welcome that points the agent at a
// directory that isn't there.
func TestSpawn_SubdirWorktree_InvalidWorktreePathRejected(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	resp := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name":          "worker",
		"cwd":           t.TempDir(),
		"worktree_path": "/no/such/worktree/anywhere",
	})
	assert.Equalf(t, http.StatusBadRequest, resp.Code,
		"bad worktree_path should 400; body=%s", resp.Raw)
	assert.Containsf(t, string(resp.Raw), "does not exist",
		"error should explain the worktree dir is missing; got %s", resp.Raw)
}

// Scenario: an ordinary spawn — no worktree_path at all — is
// unaffected. The welcome carries no worktree sentence; the agent
// just launches in its cwd as before.
func TestSpawn_SubdirWorktree_OmittedLeavesWelcomeClean(t *testing.T) {
	f := newFlow(t)
	f.HaveGroup("alpha")

	spawn := f.AsHuman().SpawnWith("alpha", map[string]any{
		"name": "worker",
		"cwd":  t.TempDir(),
	})
	require.Equalf(t, http.StatusOK, spawn.Code, "spawn body=%s", spawn.Raw)

	// Wait for the welcome itself (it carries this marker), then confirm
	// it said nothing about a worktree.
	f.AssertSpawnInitialPrompt(spawn.ConvID, "spawned by the human", 10*time.Second)
	prompt, _ := f.World.SpawnInitialPrompt(spawn.ConvID)
	assert.NotContainsf(t, prompt, "git worktree for code changes",
		"a worktree-free spawn must not mention a worktree; got %q", prompt)
}

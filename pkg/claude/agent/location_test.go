package agent

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TestResolveLocation exercises the startup-vs-current combination
// logic: which store each field comes from, the "current mirrors
// startup until the first edit" default, and the pre-v28 fallback
// where a hook row carries no worktree_root. The conv_index-sourced
// startup branch is covered end-to-end by the agentd flow tests.
func TestResolveLocation(t *testing.T) {
	setupTestDB(t)

	// Nothing known about the conv → an empty Location, not a panic.
	loc := ResolveLocation("ghost-conv")
	assert.Empty(t, loc.StartupDir)
	assert.False(t, loc.Tracked)
	assert.False(t, loc.Moved())

	const conv = "loc-conv-1"
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "s1", TmuxSession: "t1", ConvID: conv,
		Cwd: "/home/u/git/monorepo", Status: "running",
	}))

	// Session row, no edits yet → current mirrors startup.
	loc = ResolveLocation(conv)
	assert.Equal(t, "/home/u/git/monorepo", loc.StartupDir)
	assert.Equal(t, "/home/u/git/monorepo", loc.CurrentDir,
		"current mirrors startup before any edit")
	assert.False(t, loc.Tracked)
	assert.False(t, loc.Moved())

	// The agent edits a file in a sub-repo worktree — the hook records
	// the edit dir, its worktree root, and the branch.
	require.NoError(t, db.UpsertAgentWorkdir(conv,
		"/home/u/git/monorepo/svc/api/pkg", "/home/u/git/monorepo/svc/api", "feature-x"))
	loc = ResolveLocation(conv)
	assert.True(t, loc.Tracked)
	assert.Equal(t, "/home/u/git/monorepo", loc.StartupDir, "startup dir unchanged")
	assert.Equal(t, "/home/u/git/monorepo/svc/api/pkg", loc.EditDir)
	assert.Equal(t, "/home/u/git/monorepo/svc/api", loc.CurrentDir, "current = worktree root")
	assert.Equal(t, "feature-x", loc.CurrentBranch)
	assert.True(t, loc.Moved(), "agent has moved into a sub-repo")

	// A row with no worktree_root and an edit dir that isn't a
	// resolvable git repo (this path doesn't exist on disk) — the
	// on-demand resolution finds nothing, so CurrentDir falls back to
	// the edit dir rather than going blank. A stale row pointing at a
	// real repo is covered end-to-end by the agentd flow tests, which
	// can stand up an actual git repo on disk.
	require.NoError(t, db.UpsertAgentWorkdir(conv,
		"/home/u/git/monorepo/svc/api/pkg", "", ""))
	loc = ResolveLocation(conv)
	assert.True(t, loc.Tracked)
	assert.Equal(t, "/home/u/git/monorepo/svc/api/pkg", loc.CurrentDir,
		"current falls back to the edit dir when no git repo resolves")
	assert.Empty(t, loc.CurrentBranch)
}

// TestResolveLocation_WorkspaceFreshensLaunchBranch pins the statusbar
// fix: an agent_workspace row written after conv_index supersedes
// conv_index.git_branch as the launch-dir CurrentBranch. This is the
// "branch flipped but the dashboard stayed on the previous one for
// minutes" lag the user reported — a `git checkout` in an idle
// session's launch dir reaches the dashboard via the statusbar's
// render-cadence write, no .jsonl turn required.
func TestResolveLocation_WorkspaceFreshensLaunchBranch(t *testing.T) {
	setupTestDB(t)
	const conv = "loc-ws-1"

	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "ws-s1", TmuxSession: "ws-t1", ConvID: conv,
		Cwd: "/repo", Status: "running",
	}))

	// conv_index stamps the old branch at t0.
	t0 := time.Now().Add(-time.Hour)
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:           conv,
		ProjectDir:       "/repo",
		ProjectPath:      "/repo",
		GitBranch:        "old-branch",
		GitBranchStartup: "old-branch",
		IndexedAt:        t0,
	}))
	loc := ResolveLocation(conv)
	assert.Equal(t, "old-branch", loc.CurrentBranch, "conv_index seeds the launch-dir branch")

	// Statusbar writes a fresher row claiming the new branch.
	require.NoError(t, db.UpsertAgentWorkspace(db.AgentWorkspace{
		ConvID:    conv,
		Cwd:       "/repo",
		Branch:    "new-branch",
		UpdatedAt: t0.Add(30 * time.Minute),
	}))
	loc = ResolveLocation(conv)
	assert.Equal(t, "new-branch", loc.CurrentBranch,
		"fresher agent_workspace.branch supersedes conv_index for the launch-dir case")
	assert.Equal(t, "old-branch", loc.StartupBranch,
		"StartupBranch stays the first-turn branch — immutable")

	// Now conv_index updates (new turn appended) AFTER the workspace
	// row — it wins again.
	require.NoError(t, db.UpsertConvIndex(&db.ConvIndexRow{
		ConvID:           conv,
		ProjectDir:       "/repo",
		ProjectPath:      "/repo",
		GitBranch:        "post-turn-branch",
		GitBranchStartup: "old-branch",
		IndexedAt:        t0.Add(45 * time.Minute),
	}))
	loc = ResolveLocation(conv)
	assert.Equal(t, "post-turn-branch", loc.CurrentBranch,
		"newer conv_index reclaims authority when its timestamp is more recent")
}

// TestResolveLocation_WorkspaceSkippedWhenMoved guards the worktree
// case: agent_workspace only sees CC's launch dir, never the worktree
// the agent has Bash'ed into, so for a moved agent the PostToolUse hook
// (agent_workdir) remains the only writer that can describe the right
// CurrentBranch — agent_workspace must not clobber it, no matter how
// recent its timestamp.
func TestResolveLocation_WorkspaceSkippedWhenMoved(t *testing.T) {
	setupTestDB(t)
	const conv = "loc-ws-moved"

	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "wm-s1", TmuxSession: "wm-t1", ConvID: conv,
		Cwd: "/launch", Status: "running",
	}))
	require.NoError(t, db.UpsertAgentWorkdir(conv,
		"/worktree/api/pkg", "/worktree/api", "feature-x"))
	// A very fresh statusbar row claiming the launch dir's branch.
	require.NoError(t, db.UpsertAgentWorkspace(db.AgentWorkspace{
		ConvID:    conv,
		Cwd:       "/launch",
		Branch:    "main",
		UpdatedAt: time.Now(),
	}))

	loc := ResolveLocation(conv)
	assert.True(t, loc.Moved(), "agent has moved into a worktree")
	assert.Equal(t, "/worktree/api", loc.CurrentDir,
		"CurrentDir stays the worktree — agent_workspace can't see it")
	assert.Equal(t, "feature-x", loc.CurrentBranch,
		"CurrentBranch stays the worktree's — agent_workspace can't override it for a moved agent")
}

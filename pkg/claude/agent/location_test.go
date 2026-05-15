package agent

import (
	"testing"

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

	// A row last written by a pre-v28 hook carries no worktree_root —
	// CurrentDir falls back to the edit dir rather than going blank.
	require.NoError(t, db.UpsertAgentWorkdir(conv,
		"/home/u/git/monorepo/svc/api/pkg", "", ""))
	loc = ResolveLocation(conv)
	assert.True(t, loc.Tracked)
	assert.Equal(t, "/home/u/git/monorepo/svc/api/pkg", loc.CurrentDir,
		"current falls back to the edit dir when worktree_root is unset")
	assert.Empty(t, loc.CurrentBranch)
}

package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: an agent is launched in a "virtual monorepo" (a plain
// directory, not a git repo) and then edits files inside a git
// worktree of a nested sub-repo. The PostToolUse hook records that
// worktree root + branch into agent_workdir.
//
// Expected: every agent-listing surface reports the split — the launch
// dir as startup, the sub-repo worktree as current — so the human sees
// where the agent actually is, not just where it started. This is the
// robustness contract for agents that hop between directories: a
// renamed column or a dropped struct field on any surface fails here.
func TestAgentLocation_StartupVsCurrentSurfaced(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	f := newFlow(t)

	const conv = "loc1aaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	const monorepo = "/home/u/git/monorepo"
	const worktree = "/home/u/git/monorepo/svc/api-feature-x"

	f.HaveGroup("squad")
	// Launched in the monorepo. A monorepo dir isn't a git repo, so
	// Claude Code stamps no gitBranch — the startup branch is empty.
	f.HaveAliveSessionOnBranch(conv, "lbl-loc1", "tmux-loc1", monorepo, "")
	f.HaveMember("squad", conv, "worker")

	// The PostToolUse hook recorded an edit inside the sub-repo
	// worktree: the edit dir, its worktree root, and the branch.
	require.NoError(t, db.UpsertAgentWorkdir(conv, worktree+"/pkg", worktree, "feature-x"),
		"seed agent_workdir")

	// Surface 1: GET /v1/groups/squad/members.
	var m *testharness.MemberView
	for _, mm := range f.ListGroupMembers("squad") {
		mm := mm
		if mm.ConvID == conv {
			m = &mm
		}
	}
	require.NotNil(t, m, "conv not listed in group members")
	assert.Equal(t, monorepo, m.StartupDir, "members: startup_dir = launch dir")
	assert.Equal(t, worktree, m.CurrentDir, "members: current_dir = sub-repo worktree")
	assert.Equal(t, "feature-x", m.Branch, "members: branch = current branch")
	assert.Empty(t, m.StartupBranch, "members: startup branch empty for a monorepo launch dir")

	// Surface 2: GET /api/snapshot — dashboard groups + agents tabs.
	snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())

	var dm *dashMember
	for i := range snap.Groups {
		if snap.Groups[i].Name != "squad" {
			continue
		}
		for j := range snap.Groups[i].Members {
			if snap.Groups[i].Members[j].ConvID == conv {
				dm = &snap.Groups[i].Members[j]
			}
		}
	}
	require.NotNil(t, dm, "conv not in dashboard group squad")
	assert.Equal(t, monorepo, dm.StartupDir, "dashboard member startup_dir")
	assert.Equal(t, worktree, dm.CurrentDir, "dashboard member current_dir")
	assert.Equal(t, "feature-x", dm.Branch, "dashboard member branch")

	var da *dashAgent
	for i := range snap.Agents {
		if snap.Agents[i].ConvID == conv {
			da = &snap.Agents[i]
		}
	}
	require.NotNil(t, da, "conv not in dashboard agents tab")
	assert.Equal(t, monorepo, da.StartupDir, "dashboard agent startup_dir")
	assert.Equal(t, worktree, da.CurrentDir, "dashboard agent current_dir")
}

// Scenario: an agent edits in two different sub-repos over its life.
// The hook overwrites agent_workdir on every edit, so the listings
// must follow the LATEST one — that's what "picks up directory changes
// during work" means in practice.
func TestAgentLocation_FollowsLatestEdit(t *testing.T) {
	f := newFlow(t)

	const conv = "loc2aaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	const monorepo = "/home/u/git/monorepo"

	f.HaveGroup("squad")
	f.HaveAliveSessionOnBranch(conv, "lbl-loc2", "tmux-loc2", monorepo, "")
	f.HaveMember("squad", conv, "rover")

	// First the agent works in svc/api...
	require.NoError(t, db.UpsertAgentWorkdir(conv,
		monorepo+"/svc/api/pkg", monorepo+"/svc/api", "api-fix"))
	branchFor := func(convID string) (current, dir string) {
		for _, mm := range f.ListGroupMembers("squad") {
			if mm.ConvID == convID {
				return mm.Branch, mm.CurrentDir
			}
		}
		return "", ""
	}
	cur, dir := branchFor(conv)
	assert.Equal(t, "api-fix", cur, "branch after first edit")
	assert.Equal(t, monorepo+"/svc/api", dir, "dir after first edit")

	// ...then it hops to svc/web. The next listing must follow.
	require.NoError(t, db.UpsertAgentWorkdir(conv,
		monorepo+"/svc/web/src", monorepo+"/svc/web", "web-feature"))
	cur, dir = branchFor(conv)
	assert.Equal(t, "web-feature", cur, "branch follows the hop to svc/web")
	assert.Equal(t, monorepo+"/svc/web", dir, "dir follows the hop to svc/web")
}

package agentd_test

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario: an agent has been running on one branch; the user does
// `git checkout other-branch` in the launch dir; the .jsonl hasn't been
// appended yet (the agent is idle). The dashboard's pre-statusbar
// reality was that conv_index.git_branch stayed on the previous branch
// until the next turn — many minutes of stale data while the statusbar
// in the terminal already showed the new branch.
//
// The statusbar now publishes an agent_workspace row on every Claude
// Code render. /api/snapshot must reflect the new branch by reading
// that row, not by waiting for a .jsonl turn.
//
// Pins the user-reported bug end to end through the real ResolveLocation
// → locationView → /api/snapshot path.
func TestDashboardSnapshot_BranchFlipsViaStatusbarWithoutTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
		t.Cleanup(restoreURL)

		// Fake the gh/git resolver so branchLinksFor's bl_ cache doesn't
		// time-travel out: a new branch's PR resolves to PR #99.
		t.Cleanup(agentd.SetGitInfoResolverForTest(
			func(repoDir, branch string) (string, string, int, string, string, bool) {
				switch branch {
				case "feature-new":
					return "https://github.com/o/r", "main", 99,
						"https://github.com/o/r/pull/99", "open", true
				case "feature-old":
					return "https://github.com/o/r", "main", 42,
						"https://github.com/o/r/pull/42", "open", true
				}
				return "https://github.com/o/r", "main", 0, "", "", true
			}))

		const conv = "wsff0000-0000-0000-0000-000000000001"
		f := newFlow(t)
		f.HaveGroup("squad")
		f.HaveAliveSessionOnBranch(conv, "spwn-wsff", "tmux-wsff", "/repo", "feature-old")
		f.HaveMember("squad", conv)

		// Force a conv_index scan via the same surface ResolveLocation reads
		// (the dashboard refreshes through this).
		require.NotNil(t, agent.FreshConvRowResolved(conv), "conv_index scan")

		mux := agentd.BuildDashboardHandlerForTest()

		// Baseline: snapshot reports the original branch from conv_index.
		snap := fetchDashSnapshot(t, mux)
		row := findAgent(snap.Agents, conv)
		require.NotNil(t, row, "conv on the agents tab")
		require.Equal(t, "feature-old", row.Branch, "starts on the launch-dir branch")

		// Let the (fake) clock advance so the workspace write lands strictly
		// after conv_index's IndexedAt — modelling the real gap between the
		// launch-dir scan and the later statusbar render. Under wall-clock
		// these two time.Now() calls differ by microseconds; under the
		// synctest bubble the clock is frozen between them unless advanced,
		// which would make UpdatedAt.After(launchBranchTs) false.
		time.Sleep(time.Second)

		// The user has flipped branch in the launch dir. The statusbar (CC's
		// next render) publishes the new branch + PR snapshot — no .jsonl
		// turn has been written.
		require.NoError(t, db.UpsertAgentWorkspace(db.AgentWorkspace{
			ConvID:        conv,
			Cwd:           "/repo",
			Branch:        "feature-new",
			RepoURL:       "https://github.com/o/r",
			DefaultBranch: "main",
			PRNumber:      99,
			PRURL:         "https://github.com/o/r/pull/99",
			PRState:       "open",
			UpdatedAt:     time.Now(),
		}))

		// The very next snapshot reflects the new branch via the
		// agent_workspace path — no async refresh needed for ResolveLocation,
		// no turn append needed for conv_index.
		snap = fetchDashSnapshot(t, mux)
		row = findAgent(snap.Agents, conv)
		require.NotNil(t, row)
		assert.Equal(t, "feature-new", row.Branch,
			"agent_workspace supersedes conv_index for an idle launch-dir branch flip")

		// agent_workspace's repo+PR ride along — the snapshot's branch link
		// must reflect feature-new immediately, not stall on the bl_ cache.
		assert.Equal(t, "https://github.com/o/r/compare/main...feature-new", row.BranchURL,
			"workspace's RepoURL feeds the branch web link without waiting for bl_")
		assert.Equal(t, 99, row.BranchPRNum,
			"workspace's PR number rides into the snapshot row")
		assert.Equal(t, "https://github.com/o/r/pull/99", row.BranchPRURL,
			"workspace's PR URL rides into the snapshot row")
	})
}

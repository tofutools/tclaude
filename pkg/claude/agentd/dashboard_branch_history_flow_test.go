package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// Scenario: an agent works on a feature branch that has an open PR. The
// branch-history feature has two halves and this pins both end to end:
//
//  1. A conversation re-scan records the branch in conv_branch_history
//     (agent.FreshConvRowResolved → convops.ScanAndUpsertFile →
//     RebuildConvBranchHistoryScan).
//  2. The dashboard's branch-link resolver — which already shells out
//     to gh for the agent's branch — stamps the PR snapshot onto that
//     same history row (/api/snapshot → refreshBranchLink →
//     SetConvBranchHistoryPR).
//
// git + gh are a subprocess boundary, so the test swaps in the same
// deterministic resolver fake the branch-links scenario uses, then
// drives the two-phase cache resolution: the first snapshot is a cold
// miss that kicks the async resolve; after draining it the PR snapshot
// has landed on the history row. A scan that forgot to rebuild the
// history, or a resolver that forgot the PR stamp, fails here.
func TestConvBranchHistory_ScanThenPRStamp(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	const conv = "aaaaaaaa-bbbb-cccc-dddd-00000000beef"
	const cwd = "/tmp/wt/payments"
	const branch = "feature-payments"

	t.Cleanup(agentd.SetGitInfoResolverForTest(
		func(repoDir, branch string) (string, string, int, string, string, bool) {
			if branch == "feature-payments" {
				return "https://github.com/acme/app", "main", 77,
					"https://github.com/acme/app/pull/77", "open", true
			}
			return "", "", 0, "", "", false
		}))

	f := newFlow(t)
	f.HaveGroup("pay-team")
	f.HaveAliveSessionOnBranch(conv, "spwn-pay", "tmux-pay", cwd, branch)
	f.HaveMember("pay-team", conv)

	// Phase 1: the conv re-scan populates conv_branch_history off the
	// .jsonl turns — one 'scan' row, no PR resolved yet.
	require.NotNil(t, agent.FreshConvRowResolved(conv), "conv_index scan")

	rows, err := db.ListConvBranchHistory(conv)
	require.NoError(t, err)
	require.Len(t, rows, 1, "the re-scan recorded the branch")
	assert.Equal(t, branch, rows[0].Branch)
	assert.Equal(t, db.BranchSourceScan, rows[0].Source)
	assert.Zero(t, rows[0].PRNumber, "no PR before the snapshot resolver runs")

	mux := agentd.BuildDashboardHandlerForTest()

	// Phase 2: the first snapshot is a cold cache miss that kicks the
	// async branch-link resolve; draining it lets the PR stamp land.
	_ = fetchDashSnapshot(t, mux)
	agentd.WaitForBackgroundForTest()

	rows, err = db.ListConvBranchHistory(conv)
	require.NoError(t, err)
	require.Len(t, rows, 1, "still one row — the PR stamp updates, not inserts")
	assert.Equal(t, 77, rows[0].PRNumber, "PR number stamped from the resolver")
	assert.Equal(t, "https://github.com/acme/app/pull/77", rows[0].PRURL)
	assert.Equal(t, "open", rows[0].PRState)
	assert.Equal(t, db.BranchSourceScan, rows[0].Source, "the PR stamp leaves source intact")
}

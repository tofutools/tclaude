package agentd_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
)

// Scenario: two agents working on feature branches of a GitHub repo —
// one with an open PR, one without. The dashboard's Branch column
// renders clickable links, so /api/snapshot must carry, per row, a
// branch web URL and (when one exists) the branch's PR number + URL.
//
// git + gh are a subprocess + network boundary, so ResolveLocation
// stays a pure DB read and the snapshot path never shells out: it
// reads a DB-backed cache and schedules an async refresh on a miss.
// The test swaps the git/gh resolver for a deterministic fake — the
// same pattern as the tmux / spawner seams — then drives the two-phase
// resolution it forces: the first snapshot is a cold cache miss that
// kicks the background resolve; after draining it, the second snapshot
// reads the populated cache.
//
// Pins the wiring end to end: a dropped repoLinksView field, a broken
// cache key, or a snapshot that forgot to call branchLinksFor all fail
// here, on the real /api/snapshot surface the dashboard renders from.
func TestDashboardBranchLinks_SurfacedInSnapshot(t *testing.T) {
	restoreURL := agentd.SetPopupBaseURLForTest("http://127.0.0.1:0")
	t.Cleanup(restoreURL)

	const aliceConv = "aaaaaaaa-bbbb-cccc-dddd-000000000001"
	const bobConv = "bbbbbbbb-bbbb-cccc-dddd-000000000002"

	// Fake resolver: alice's branch has PR #42, bob's branch has none.
	// An unknown branch models a non-GitHub repo (ok=false).
	t.Cleanup(agentd.SetGitInfoResolverForTest(
		func(repoDir, branch string) (string, string, int, string, bool) {
			switch branch {
			case "feature-login":
				return "https://github.com/acme/app", "main", 42,
					"https://github.com/acme/app/pull/42", true
			case "bugfix-crash":
				return "https://github.com/acme/app", "main", 0, "", true
			}
			return "", "", 0, "", false
		}))

	f := newFlow(t)
	f.HaveGroup("squad")
	f.HaveAliveSessionOnBranch(aliceConv, "spwn-alice", "tmux-alice", "/tmp/wt/login", "feature-login")
	f.HaveAliveSessionOnBranch(bobConv, "spwn-bob", "tmux-bob", "/tmp/wt/crash", "bugfix-crash")
	f.HaveMember("squad", aliceConv, "alice")
	f.HaveMember("squad", bobConv, "bob")

	// Stand in for the watch model: scan each conv's .jsonl into
	// conv_index so ResolveLocation reads the branch off the cached row.
	require.NotNil(t, agent.FreshConvRowResolved(aliceConv), "alice conv_index scan")
	require.NotNil(t, agent.FreshConvRowResolved(bobConv), "bob conv_index scan")

	mux := agentd.BuildDashboardHandlerForTest()

	// First snapshot: cold cache miss — links empty, async resolve kicked.
	_ = fetchDashSnapshot(t, mux)
	agentd.WaitForBackgroundForTest() // drain the resolve goroutines

	// Second snapshot: cache populated — links present.
	snap := fetchDashSnapshot(t, mux)

	alice := findAgent(snap.Agents, aliceConv)
	require.NotNil(t, alice, "alice on the agents tab")
	assert.Equal(t, "https://github.com/acme/app/compare/main...feature-login",
		alice.BranchURL, "alice branch compare URL")
	assert.Equal(t, 42, alice.BranchPRNum, "alice PR number")
	assert.Equal(t, "https://github.com/acme/app/pull/42", alice.BranchPRURL, "alice PR URL")

	bob := findAgent(snap.Agents, bobConv)
	require.NotNil(t, bob, "bob on the agents tab")
	assert.Equal(t, "https://github.com/acme/app/compare/main...bugfix-crash",
		bob.BranchURL, "bob branch compare URL")
	assert.Zero(t, bob.BranchPRNum, "bob has no PR")
	assert.Empty(t, bob.BranchPRURL, "bob has no PR URL")

	// The same links must surface on the groups-tab member rows — both
	// the Groups and Agents tabs render through branchCell().
	var squad *dashGroup
	for i := range snap.Groups {
		if snap.Groups[i].Name == "squad" {
			squad = &snap.Groups[i]
		}
	}
	require.NotNil(t, squad, "snapshot missing group squad")
	var aliceMember *dashMember
	for i := range squad.Members {
		if squad.Members[i].ConvID == aliceConv {
			aliceMember = &squad.Members[i]
		}
	}
	require.NotNil(t, aliceMember, "alice on the groups tab")
	assert.Equal(t, "https://github.com/acme/app/compare/main...feature-login",
		aliceMember.BranchURL, "groups-tab branch URL")
	assert.Equal(t, 42, aliceMember.BranchPRNum, "groups-tab PR number")
}

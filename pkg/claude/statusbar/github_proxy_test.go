package statusbar

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TestPickBranchPRPrefersTheOpenOne — a branch outlives its pull requests: an
// attempt gets closed, another is opened from the same head. The rows arrive
// newest-created first, and the bar must show the LIVE one rather than
// whichever was created last, which is the rule `gh pr view <branch>` follows.
func TestPickBranchPRPrefersTheOpenOne(t *testing.T) {
	prs := []ghProxyPREntry{
		{Number: 9, State: "CLOSED", URL: "u9", HeadRefName: "feat"},
		{Number: 7, State: "OPEN", URL: "u7", HeadRefName: "feat"},
		{Number: 3, State: "MERGED", URL: "u3", HeadRefName: "feat"},
	}
	got := pickBranchPR(prs, "feat")
	require.NotNil(t, got)
	assert.Equal(t, 7, got.Number)
}

// TestPickBranchPRFallsBackToTheNewest — with nothing open, the branch's
// history is still worth a link, and the newest is the one the agent just
// worked on. A merged PR renders with its state; dropping it would make the
// link vanish the moment the work landed.
func TestPickBranchPRFallsBackToTheNewest(t *testing.T) {
	prs := []ghProxyPREntry{
		{Number: 9, State: "MERGED", URL: "u9", HeadRefName: "feat"},
		{Number: 3, State: "CLOSED", URL: "u3", HeadRefName: "feat"},
	}
	got := pickBranchPR(prs, "feat")
	require.NotNil(t, got)
	assert.Equal(t, 9, got.Number)
}

// TestPickBranchPRIgnoresAnotherBranchesPR — the daemon filters on the head
// name, so this only fires if that filter is ever widened. Showing a
// neighbouring branch's PR in this branch's bar would be worse than showing
// none: it reads as "my work has a PR" when it does not.
func TestPickBranchPRIgnoresAnotherBranchesPR(t *testing.T) {
	prs := []ghProxyPREntry{{Number: 9, State: "OPEN", URL: "u9", HeadRefName: "other"}}
	assert.Nil(t, pickBranchPR(prs, "feat"))
	assert.Nil(t, pickBranchPR(nil, "feat"))
}

// TestPRLookupTTLIsSlowerThroughTheProxy pins the whole reason the snapshot
// carries a PRVia at all. A proxied lookup spends the OPERATOR's GitHub
// credential and writes an audit row; a status line re-renders several times a
// second. If these two ever became equal, a handful of panes would turn the
// operator's audit trail into a render log.
func TestPRLookupTTLIsSlowerThroughTheProxy(t *testing.T) {
	assert.Equal(t, gitCacheTTL, prLookupTTL(prViaGH))
	assert.Equal(t, gitCacheTTL, prLookupTTL(""), "an entry from before PRVia existed keeps the old cadence")
	assert.Greater(t, prLookupTTL(prViaProxy), gitCacheTTL)
}

// TestCarryPRForwardKeepsARecentProxyLookup — the carry-forward IS the
// throttle. Without it the proxied path would spend a credentialed call on
// every 15-second snapshot refresh, which is the cost prLookupTTL exists to
// avoid.
func TestCarryPRForwardKeepsARecentProxyLookup(t *testing.T) {
	cached := &GitSnapshot{
		Branch:      "feat",
		PRNumber:    42,
		PRURL:       "https://github.com/o/r/pull/42",
		PRState:     "open",
		PRFetchedAt: time.Now().Add(-30 * time.Second),
		PRVia:       prViaProxy,
	}
	data := &GitSnapshot{Branch: "feat", FetchedAt: time.Now()}

	require.True(t, carryPRForward(cached, data))
	assert.Equal(t, 42, data.PRNumber)
	assert.Equal(t, "open", data.PRState)
	assert.Equal(t, cached.PRFetchedAt, data.PRFetchedAt,
		"carrying a lookup forward must not restamp it as newly observed")
	assert.Equal(t, prViaProxy, data.PRVia)
}

// TestCarryPRForwardKeepsANegativeResult — "this branch has no pull request"
// is the answer for every freshly-pushed feature branch, it costs exactly as
// much to obtain as a positive one, and re-asking it every fifteen seconds is
// the most expensive way to keep learning nothing.
func TestCarryPRForwardKeepsANegativeResult(t *testing.T) {
	cached := &GitSnapshot{
		Branch:      "feat",
		PRFetchedAt: time.Now().Add(-30 * time.Second),
		PRVia:       prViaProxy,
	}
	data := &GitSnapshot{Branch: "feat", FetchedAt: time.Now()}

	assert.True(t, carryPRForward(cached, data))
	assert.Zero(t, data.PRNumber)
}

// TestCarryPRForwardRefusesWhatItCannotVouchFor. Each of these would put a PR
// on the bar that is not this branch's, or hide a change behind a stale one.
func TestCarryPRForwardRefusesWhatItCannotVouchFor(t *testing.T) {
	now := time.Now()
	cases := map[string]*GitSnapshot{
		"no previous snapshot": nil,
		"never looked up": {
			Branch: "feat", PRNumber: 42, PRVia: prViaProxy,
		},
		"a different branch": {
			Branch: "other", PRNumber: 42, PRFetchedAt: now.Add(-time.Second), PRVia: prViaProxy,
		},
		"older than the proxy cadence": {
			Branch: "feat", PRNumber: 42, PRFetchedAt: now.Add(-2 * proxyPRLookupTTL), PRVia: prViaProxy,
		},
		"older than the gh cadence": {
			Branch: "feat", PRNumber: 42, PRFetchedAt: now.Add(-2 * gitCacheTTL), PRVia: prViaGH,
		},
		// A clock that jumped backwards makes the age meaningless, and the
		// safe reading of a meaningless age is "look it up again".
		"stamped in the future": {
			Branch: "feat", PRNumber: 42, PRFetchedAt: now.Add(time.Hour), PRVia: prViaProxy,
		},
	}
	for name, cached := range cases {
		t.Run(name, func(t *testing.T) {
			data := &GitSnapshot{Branch: "feat", FetchedAt: now}
			assert.False(t, carryPRForward(cached, data))
			assert.Zero(t, data.PRNumber)
		})
	}
}

// TestApplyRenderWritesPublishesThePRObservationTime — agentd compares this
// column against its OWN pull-request observation and shows whichever is
// newer. A snapshot whose PR was carried forward from a lookup a minute ago
// must not claim the freshness of the git facts gathered around it, or a stale
// state wins against the daemon's fresher one.
func TestApplyRenderWritesPublishesThePRObservationTime(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	const conv = "statusline-pr-observed-at"
	prFetchedAt := time.Now().Add(-80 * time.Second).Truncate(time.Microsecond)
	input := StatusLineInput{}
	input.Workspace.CurrentDir = "/repo"

	require.True(t, applyRenderWrites(renderWrites{
		Input:         input,
		WorkspaceConv: conv,
		Git: &GitSnapshot{
			RepoURL:       "https://github.com/o/r",
			Branch:        "feature",
			DefaultBranch: "main",
			PRNumber:      42,
			PRURL:         "https://github.com/o/r/pull/42",
			PRState:       "open",
			FetchedAt:     time.Now(),
			PRFetchedAt:   prFetchedAt,
			PRVia:         prViaProxy,
		},
	}))

	ws, err := db.GetAgentWorkspace(conv)
	require.NoError(t, err)
	assert.WithinDuration(t, prFetchedAt, ws.UpdatedAt, time.Microsecond,
		"the published freshness clock must be when the PR was looked up, not when the snapshot was gathered")
}

// TestGitHubProxyEnabledIsFalseWithoutADaemon — the gate fails closed onto the
// behaviour that cannot regress anyone: `gh`, exactly as before. No daemon, an
// older daemon that does not project the capability, a socket that will not
// dial — all of them mean "not through the proxy".
func TestGitHubProxyEnabledIsFalseWithoutADaemon(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	// An absolute override is authoritative in ClientSocketPaths, so this
	// pins the probe to a socket that does not exist rather than letting it
	// find whatever daemon the developer running the tests has going.
	t.Setenv(agentipc.SocketEnv, filepath.Join(dir, "no-such-agentd.sock"))
	assert.False(t, githubProxyEnabled())

	_, _, _, ok := proxyPRInfo("feat")
	assert.False(t, ok, "an unreachable daemon must not be read as 'this branch has no PR'")
}

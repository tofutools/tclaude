package agentd_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// Scenario: an agent is on a feature branch with an open PR, and the operator
// hovers that PR's badge in the Groups tab.
//
// The hover is the only path that spends a subprocess on CI state outside the
// piggybacked branch-link refresh, so this drives the real one: GET
// /api/pr-checks schedules the resolve, the resolve lands in the shared
// per-PR cache, and the NEXT dashboard snapshot serves the summary on the
// branch badge. That last hop is the load-bearing one — a snapshot that never
// read the cache would leave the indicator permanently blank no matter how
// well the endpoint worked.
func TestDashboardPRChecks_HoverRefreshReachesSnapshotBadge(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	const (
		conv   = "prchecks-0000-0000-0000-000000000001"
		branch = "feature-ci-badges"
		prURL  = "https://github.com/acme/app/pull/77"
	)

	t.Cleanup(agentd.SetGitInfoResolverForTest(
		func(_, b string) (string, string, int, string, string, bool) {
			if b != branch {
				return "", "", 0, "", "", false
			}
			return "https://github.com/acme/app", "main", 77, prURL, "open", true
		}))
	rollupCalls := 0
	t.Cleanup(agentd.SetPRChecksResolverForTest(func(rawURL string) (string, bool) {
		rollupCalls++
		assert.Equal(t, prURL, rawURL, "the hover must resolve the PR it was opened on")
		return `[
			{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"FAILURE",
			 "workflowName":"CI","detailsUrl":"https://github.com/acme/app/runs/1",
			 "startedAt":"2026-08-09T10:00:00Z","completedAt":"2026-08-09T10:03:12Z"},
			{"__typename":"CheckRun","name":"lint","status":"IN_PROGRESS","workflowName":"CI",
			 "startedAt":"2026-08-09T10:00:00Z"},
			{"__typename":"CheckRun","name":"build","status":"COMPLETED","conclusion":"SUCCESS"},
			{"__typename":"CheckRun","name":"docs","status":"COMPLETED","conclusion":"SKIPPED"}
		]`, true
	}))

	f := newFlow(t)
	f.HaveGroup("squad")
	f.HaveAliveSessionOnBranch(conv, "spwn-prchecks", "tmux-prchecks", f.TestCwd("wt/ci"), branch)
	f.HaveMember("squad", conv)
	require.NotNil(t, agent.FreshConvRowResolved(conv), "conv_index scan")

	mux := agentd.BuildDashboardHandlerForTest()

	// Cold snapshot: the branch link resolves asynchronously and carries no
	// checks yet, so no indicator is drawn at all — an empty badge would be
	// worse than none.
	_ = fetchDashSnapshot(t, mux)
	agentd.WaitForBackgroundForTest()
	cold := findAgent(fetchDashSnapshot(t, mux).Agents, conv)
	require.NotNil(t, cold)
	require.Equal(t, prURL, cold.BranchPRURL, "branch PR must resolve before the badge can")
	assert.Nil(t, cold.BranchChecks, "an unresolved PR shows no CI indicator")

	// The operator hovers: the panel's endpoint answers immediately from the
	// (empty) cache and schedules the resolve behind it.
	first := fetchPRChecks(t, mux, prURL)
	assert.False(t, first.Resolved, "the first hover has nothing cached yet")
	assert.True(t, first.Refreshing, "and must have scheduled the refresh the human asked for")
	agentd.WaitForBackgroundForTest()

	// The next poll of the same endpoint — the panel re-polls while the
	// pointer stays on it — serves the resolved list.
	second := fetchPRChecks(t, mux, prURL)
	require.True(t, second.Resolved)
	require.Len(t, second.Checks, 4)
	assert.Equal(t, "test", second.Checks[0].Name)
	assert.Equal(t, "fail", second.Checks[0].Bucket)
	assert.Equal(t, "CI", second.Checks[0].Source)
	assert.Equal(t, "https://github.com/acme/app/runs/1", second.Checks[0].URL)
	assert.Equal(t, "pending", second.Checks[1].Bucket)
	assert.Equal(t, "skipped", second.Checks[3].Bucket)
	assert.Equal(t, dashChecks{Total: 4, Passed: 1, Failed: 1, Pending: 1, Skipped: 1, State: "failing",
		FetchedAt: second.Summary.FetchedAt}, second.Summary)

	// And the badge itself: the snapshot reads the same cache, so the Groups
	// and Agents tabs now carry the counts without any further gh call.
	snap := fetchDashSnapshot(t, mux)
	warm := findAgent(snap.Agents, conv)
	require.NotNil(t, warm)
	require.NotNil(t, warm.BranchChecks, "the snapshot must serve the cached CI summary")
	assert.Equal(t, "failing", warm.BranchChecks.State)
	assert.Equal(t, 1, warm.BranchChecks.Passed)
	assert.Equal(t, 4, warm.BranchChecks.Total)

	member := findDashMember(snap, "squad", conv)
	require.NotNil(t, member, "agent on the groups tab")
	require.NotNil(t, member.BranchChecks, "the Groups tab badge is the whole point of the feature")
	assert.Equal(t, "failing", member.BranchChecks.State)

	assert.Equal(t, 1, rollupCalls,
		"only the hover spends a subprocess — the snapshots must read the cache")
}

// A merged or closed PR's checks are frozen, so no amount of hovering may keep
// re-polling one. The cached answer keeps being served instead.
func TestDashboardPRChecks_TerminalPRStopsPolling(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))

	const (
		worker = "prcm-aaaa-bbbb-cccc-dddd"
		prURL  = "https://github.com/acme/app/pull/91"
	)
	rollupCalls := 0
	t.Cleanup(agentd.SetPRChecksResolverForTest(func(string) (string, bool) {
		rollupCalls++
		return `[{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"SUCCESS"}]`, true
	}))
	t.Cleanup(agentd.SetPresentedPRInfoResolverForTest(
		func(rawURL string) (int, string, string, bool) { return 91, rawURL, "merged", true }))

	f.HaveGroup("alpha")
	f.HaveConvWithTitle(worker, "worker")
	f.HaveAliveSession(worker, "lbl-prcm", "tmux-prcm", f.TestCwd("prcm"))
	f.HaveMember("alpha", worker)
	require.NoError(t, db.SetAgentPermissionOverride(worker, agentd.PermSelfPR, db.PermEffectGrant, "test"))

	// The agent presents the PR; the dashboard's own refresh observes that it
	// is merged, which is what freezes its checks.
	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/prs",
			map[string]any{"url": prURL, "summary": "shipped"}), worker))
	require.Equalf(t, http.StatusOK, rec.Code, "present PR: body=%s", rec.Body.String())
	mux := agentd.BuildDashboardHandlerForTest()
	_ = fetchDashSnapshot(t, mux)
	agentd.WaitForBackgroundForTest()

	// One hover populates the cache — a merged PR the dashboard never saw
	// running still deserves one fetch, or its panel would stay empty forever.
	first := fetchPRChecks(t, mux, prURL)
	assert.True(t, first.Refreshing, "an empty cache is fetched once even for a merged PR")
	agentd.WaitForBackgroundForTest()
	require.Equal(t, 1, rollupCalls)

	// Later hovers keep serving the frozen list without spending another call.
	for range 3 {
		resp := fetchPRChecks(t, mux, prURL)
		require.True(t, resp.Resolved)
		assert.Len(t, resp.Checks, 1)
		assert.False(t, resp.Refreshing, "a merged PR must not schedule another CI poll")
	}
	agentd.WaitForBackgroundForTest()
	assert.Equal(t, 1, rollupCalls, "a terminal PR's checks are fetched once, never re-polled")
}

func TestDashboardPRChecks_RejectsNonPRURLs(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	newFlow(t)
	mux := agentd.BuildDashboardHandlerForTest()

	for _, bad := range []string{
		"", "not-a-url", "https%3A%2F%2Fexample.com%2Facme%2Fapp%2Fpull%2F1",
		"https%3A%2F%2Fgithub.com%2Facme%2Fapp", "file%3A%2F%2F%2Fetc%2Fpasswd",
	} {
		rec := testharness.Serve(mux,
			testharness.JSONRequest(t, http.MethodGet, "/api/pr-checks?url="+bad, nil))
		assert.Equal(t, http.StatusBadRequest, rec.Code, "url=%q must be refused", bad)
	}
}

type prChecksResp struct {
	URL      string     `json:"url"`
	Summary  dashChecks `json:"summary"`
	Resolved bool       `json:"resolved"`
	Stale    bool       `json:"stale"`
	// Refreshing marks a response that scheduled a background gh call.
	Refreshing bool `json:"refreshing"`
	Checks     []struct {
		Name        string `json:"name"`
		Bucket      string `json:"bucket"`
		Conclusion  string `json:"conclusion"`
		Source      string `json:"source"`
		URL         string `json:"url"`
		StartedAt   string `json:"started_at"`
		CompletedAt string `json:"completed_at"`
	} `json:"checks"`
}

func fetchPRChecks(t *testing.T, mux http.Handler, prURL string) prChecksResp {
	t.Helper()
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet,
		"/api/pr-checks?url="+url.QueryEscape(prURL), nil))
	require.Equalf(t, http.StatusOK, rec.Code, "GET /api/pr-checks: %s", rec.Body.String())
	var out prChecksResp
	testharness.DecodeJSON(t, rec, &out)
	return out
}

package agentd

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

func TestDecodeAuthoredOpenPRGraphQLSortsAttentionAndCachesChecks(t *testing.T) {
	data := []byte(`{"data":{"search":{"issueCount":4,"nodes":[
        {"number":3,"title":"Passing","url":"https://github.com/acme/app/pull/3","updatedAt":"2026-08-13T08:00:00Z","commits":{"nodes":[{"commit":{"statusCheckRollup":{"contexts":{"nodes":[{"__typename":"CheckRun","name":"test","status":"COMPLETED","conclusion":"SUCCESS"}]}}}}]}},
        {"number":1,"title":"Failing","url":"https://github.com/acme/app/pull/1","updatedAt":"2026-08-13T07:00:00Z","commits":{"nodes":[{"commit":{"statusCheckRollup":{"contexts":{"nodes":[{"__typename":"CheckRun","name":"lint","status":"COMPLETED","conclusion":"FAILURE"}]}}}}]}},
        {"number":2,"title":"No checks","url":"https://github.com/acme/other/pull/2","updatedAt":"2026-08-13T09:00:00Z","commits":{"nodes":[]}},
        {"number":4,"title":"Running clean","url":"https://github.com/acme/app/pull/4","updatedAt":"2026-08-13T10:00:00Z","commits":{"nodes":[{"commit":{"statusCheckRollup":{"contexts":{"nodes":[{"__typename":"CheckRun","name":"test","status":"IN_PROGRESS"}]}}}}]}},
        {"number":9,"title":"Reject issue URL","url":"https://github.com/acme/app/issues/9"}
      ]}}}`)
	view, checks, err := decodeAuthoredOpenPRGraphQL(data, "octocat")
	require.NoError(t, err)
	require.Len(t, view.Items, 4)
	assert.Equal(t, []int{1, 2, 3, 4}, []int{view.Items[0].Number, view.Items[1].Number, view.Items[2].Number, view.Items[3].Number},
		"clean CI still running sorts behind PRs that need attention")
	assert.Equal(t, "failing", view.Items[0].Checks.State)
	assert.Equal(t, "passing", view.Items[2].Checks.State)
	assert.Equal(t, "pending", view.Items[3].Checks.State)
	assert.Equal(t, "acme/other", view.Items[1].Repository)
	assert.Contains(t, view.SearchURL, "author%3Aoctocat")
	assert.Len(t, checks, 3)
}

func TestDecodeAuthoredRecentPRsKeepsTerminalStateOutOfTheOpenList(t *testing.T) {
	data := []byte(`{"data":{"search":{"issueCount":1,"nodes":[
        {"number":1,"title":"Open","url":"https://github.com/acme/app/pull/1","updatedAt":"2026-08-13T07:00:00Z"}
      ]},"recent":{"issueCount":3,"nodes":[
        {"number":5,"title":"Closed","url":"https://github.com/acme/app/pull/5","state":"CLOSED","closedAt":"2026-08-11T10:00:00Z"},
        {"number":7,"title":"Merged","url":"https://github.com/acme/app/pull/7","state":"MERGED","mergedAt":"2026-08-12T10:00:00Z","closedAt":"2026-08-12T10:00:00Z"},
        {"number":9,"title":"Reject issue URL","url":"https://github.com/acme/app/issues/9","state":"CLOSED"}
      ]}}}`)
	view, _, err := decodeAuthoredOpenPRGraphQL(data, "octocat")
	require.NoError(t, err)
	require.Len(t, view.Items, 1, "closed pull requests never join the open list")
	assert.Equal(t, 1, view.Total, "the open count ignores the recent search")
	require.Len(t, view.Recent, 2)
	assert.Equal(t, []int{7, 5}, []int{view.Recent[0].Number, view.Recent[1].Number}, "newest first")
	assert.Equal(t, "merged", view.Recent[0].State)
	assert.Equal(t, "2026-08-12T10:00:00Z", view.Recent[0].ClosedAt)
	assert.Equal(t, "closed", view.Recent[1].State)
	assert.False(t, view.RecentTruncated)
}

func TestDecodeAuthoredRecentPRsReportsTruncation(t *testing.T) {
	data := []byte(`{"data":{"search":{"issueCount":0,"nodes":[]},
      "recent":{"issueCount":80,"nodes":[
        {"number":5,"title":"Closed","url":"https://github.com/acme/app/pull/5","state":"MERGED","mergedAt":"2026-08-11T10:00:00Z"}
      ]}}}`)
	view, _, err := decodeAuthoredOpenPRGraphQL(data, "octocat")
	require.NoError(t, err)
	assert.True(t, view.RecentTruncated,
		"a capped page must not be presented as the complete recent count")
}

func TestAuthoredPRSearchArgsSelectQueryAndWindow(t *testing.T) {
	now := time.Date(2026, 8, 13, 3, 0, 0, 0, time.UTC)
	open := authoredPRSearchArgs("octocat", 0, now)
	assert.Contains(t, open, "query="+authoredOpenPRGraphQLQuery)
	for _, arg := range open {
		assert.NotContains(t, arg, "qr=", "the closed search is skipped when the window is off")
		assert.NotContains(t, arg, "rfirst=")
	}

	recent := authoredPRSearchArgs("octocat", 3, now)
	assert.Contains(t, recent, "query="+authoredRecentPRGraphQLQuery)
	assert.Contains(t, recent, "qr=author:octocat is:closed type:pr closed:>=2026-08-10 sort:updated-desc")
	assert.Contains(t, recent, "rfirst=50")
	assert.Contains(t, recent, "q=author:octocat is:open type:pr sort:updated-desc",
		"the open search is identical either way")

	// Both query shapes must embed the SAME open search: the two-search shape
	// is what a default configuration runs, so a divergent copy would strand
	// every default install on a stale rollup selection.
	assert.Contains(t, authoredOpenPRGraphQLQuery, authoredOpenPRSearchFragment)
	assert.Contains(t, authoredRecentPRGraphQLQuery, authoredOpenPRSearchFragment)
}

func TestFilterRecentAuthoredPRsEnforcesTheExactWindow(t *testing.T) {
	now := time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
	items := []dashboardAuthoredOpenPR{
		{Number: 1, ClosedAt: "2026-08-13T09:00:00Z"},
		{Number: 2, ClosedAt: "2026-08-09T09:00:00Z"},
		{Number: 3, ClosedAt: "not a timestamp"},
	}
	kept := filterRecentAuthoredPRs(items, 3, now)
	require.Len(t, kept, 1, "GitHub's date-granular bound is tightened to the exact instant")
	assert.Equal(t, 1, kept[0].Number)
	assert.Empty(t, filterRecentAuthoredPRs(items, 0, now), "window 0 disables the filter")
}

func TestAuthoredOpenPRSnapshotResolvesConfigAtPollTime(t *testing.T) {
	setupTestDB(t)
	now := time.Now().UTC()
	view := dashboardAuthoredOpenPRs{
		Available: true, Login: "octocat", Total: 0, RecentWindowDays: 7,
		Items: []dashboardAuthoredOpenPR{},
		Recent: []dashboardAuthoredOpenPR{
			{Number: 5, URL: "https://github.com/acme/app/pull/5", State: "merged",
				ClosedAt: now.Add(-24 * time.Hour).Format(time.RFC3339)},
			{Number: 6, URL: "https://github.com/acme/app/pull/6", State: "merged",
				ClosedAt: now.Add(-5 * 24 * time.Hour).Format(time.RFC3339)},
		},
	}
	data, err := json.Marshal(view)
	require.NoError(t, err)
	require.NoError(t, db.SaveGitCache(authoredOpenPRCacheKey("octocat"), data, time.Now()))
	setAuthoredOpenPRActiveLogin("octocat")

	def := loadAuthoredOpenPRsSnapshot(nil)
	assert.True(t, def.AlwaysShow, "the indicator is permanent by default")
	assert.Equal(t, config.RecentPRWindowDaysDefault, def.RecentWindowDays)
	require.Len(t, def.Recent, 1, "a narrowed window applies before the next GitHub search")
	assert.Equal(t, 5, def.Recent[0].Number)
	assert.Contains(t, def.RecentSearchURL, "author%3Aoctocat")

	off := false
	zero := 0
	opted := loadAuthoredOpenPRsSnapshot(&config.Config{Dashboard: &config.DashboardConfig{
		AlwaysShowOpenPRs: &off, RecentPRWindowDays: &zero,
	}})
	assert.False(t, opted.AlwaysShow)
	assert.Equal(t, 0, opted.RecentWindowDays)
	assert.Empty(t, opted.Recent)
	assert.Empty(t, opted.RecentSearchURL)
}

func TestPollAndAssociateAuthoredOpenPRs(t *testing.T) {
	setupTestDB(t)
	setAuthoredOpenPRActiveLogin("")
	previous := authoredOpenPRResolver
	t.Cleanup(func() { authoredOpenPRResolver = previous })
	authoredOpenPRResolver = func() (dashboardAuthoredOpenPRs, error) {
		return dashboardAuthoredOpenPRs{
			Login: "octocat", Total: 1,
			Items: []dashboardAuthoredOpenPR{{
				Number: 12, URL: "https://github.com/acme/app/pull/12", Title: "Ship it",
			}},
		}, nil
	}
	require.NoError(t, pollAuthoredOpenPRs())
	view := loadAuthoredOpenPRsSnapshot(nil)
	assert.True(t, view.Available)
	assert.NotEmpty(t, view.UpdatedAt)
	require.Len(t, view.Items, 1)

	view = associateAuthoredOpenPRs(view, []dashboardAgent{{
		AgentID: "agt_1", ConvID: "conv", Title: "builder",
		repoLinksView: repoLinksView{BranchPRURL: "https://github.com/ACME/App/pull/12/files"},
	}})
	assert.Equal(t, "agt_1", view.Items[0].AgentID)
	assert.Equal(t, "builder", view.Items[0].AgentTitle)

	row, err := db.LoadGitCache(authoredOpenPRCacheKey("octocat"))
	require.NoError(t, err)
	require.NotNil(t, row)
	assert.WithinDuration(t, time.Now(), row.FetchedAt, 2*time.Second)
	states := cachedPresentedPRStates([]string{"https://github.com/acme/app/pull/12"})
	assert.Equal(t, "open", states[prStateKey("https://github.com/acme/app/pull/12")].state,
		"the footer poll publishes into the shared per-PR state source")
}

func TestReconcileAuthoredOpenPRsUsesTheSameStateAndChecksAsGroups(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	const prURL = "https://github.com/acme/app/pull/12"
	view := dashboardAuthoredOpenPRs{
		Available: true, Total: 1, RecentWindowDays: 3,
		Items:  []dashboardAuthoredOpenPR{{Number: 12, URL: prURL, Title: "Ship it"}},
		Recent: []dashboardAuthoredOpenPR{},
	}
	states := make(prStateIndex)
	states.add(prURL, "merged", now)
	checks := map[string]*prChecksSummary{
		prStateKey(prURL): {Total: 2, Passed: 1, Failed: 1, State: "failing"},
	}

	got := reconcileAuthoredOpenPRs(view, states, checks, now)
	assert.Zero(t, got.Total)
	assert.Empty(t, got.Items, "a merge observed by Groups must leave the footer's open list immediately")
	require.Len(t, got.Recent, 1)
	assert.Equal(t, "merged", got.Recent[0].State)
	assert.Equal(t, "failing", got.Recent[0].Checks.State)

	links := repoLinksView{BranchPRURL: prURL, BranchPRState: "open"}.
		withFreshestPRStates(states).
		withPRChecks(checks)
	assert.Equal(t, got.Recent[0].State, links.BranchPRState)
	assert.Equal(t, got.Recent[0].Checks, links.BranchChecks)
}

func TestApplyAuthoredStatesExpiresAndReopensPresentedPRs(t *testing.T) {
	setupTestDB(t)
	const prURL = "https://github.com/acme/app/pull/12"
	agentID, _, err := db.EnsureAgentForConv("conv_1", "test")
	require.NoError(t, err)
	_, err = db.UpsertAgentPR(agentID, prURL, "Ship it", "open")
	require.NoError(t, err)

	applyAuthoredStatesToPresentedPRs(dashboardAuthoredOpenPRs{Recent: []dashboardAuthoredOpenPR{{
		URL: prURL + "/files", State: "closed",
	}}})

	all, err := db.ListUnhandledAgentPRs()
	require.NoError(t, err)
	require.Len(t, all[agentID], 1)
	assert.Equal(t, "closed", all[agentID][0].State)
	assert.WithinDuration(t, time.Now(), all[agentID][0].UpdatedAt, time.Second,
		"the terminal observation starts the normal presented-PR grace period")

	applyAuthoredStatesToPresentedPRs(dashboardAuthoredOpenPRs{Items: []dashboardAuthoredOpenPR{{
		URL: prURL,
	}}})
	all, err = db.ListUnhandledAgentPRs()
	require.NoError(t, err)
	require.Len(t, all[agentID], 1)
	assert.Equal(t, "open", all[agentID][0].State,
		"a reopened PR must not expire from the old closed observation")
}

func TestAuthoredOpenPRCacheRequiresCurrentProcessIdentity(t *testing.T) {
	setupTestDB(t)
	now := time.Now()
	data := []byte(`{"available":true,"login":"old-user","total":1,"items":[{"number":1,"title":"private","url":"https://github.com/private/repo/pull/1"}]}`)
	require.NoError(t, db.SaveGitCache(authoredOpenPRCacheKey("old-user"), data, now))
	setAuthoredOpenPRActiveLogin("")
	assert.False(t, loadAuthoredOpenPRsSnapshot(nil).Available,
		"a daemon restart must not publish the previous credential's private metadata")
	setAuthoredOpenPRActiveLogin("new-user")
	assert.False(t, loadAuthoredOpenPRsSnapshot(nil).Available,
		"a different active identity must never load the previous identity's cache")
}

func TestReconcileTruncatedPRChecksUsesAggregateState(t *testing.T) {
	failing := prChecksSummary{Total: 100, Passed: 100, State: "passing"}
	reconcileTruncatedPRChecks(&failing, "FAILURE", 125)
	assert.Equal(t, "failing", failing.State)
	assert.Equal(t, 125, failing.Total)
	assert.Equal(t, 1, failing.Failed)
	assert.Equal(t, 24, failing.Pending, "unseen non-failing contexts remain incomplete")

	pending := prChecksSummary{Total: 100, Passed: 90, Pending: 10, State: "pending"}
	reconcileTruncatedPRChecks(&pending, "PENDING", 120)
	assert.Equal(t, 30, pending.Pending)

	passing := prChecksSummary{Total: 100, Passed: 100, State: "passing"}
	reconcileTruncatedPRChecks(&passing, "SUCCESS", 120)
	assert.Equal(t, 120, passing.Passed)
}

func TestAuthoredOpenPRRetryDelayCaps(t *testing.T) {
	assert.Equal(t, 10*time.Second, authoredOpenPRRetryDelay(0))
	assert.Equal(t, 20*time.Second, authoredOpenPRRetryDelay(1))
	assert.Equal(t, 5*time.Minute, authoredOpenPRRetryDelay(5))
	assert.Equal(t, 5*time.Minute, authoredOpenPRRetryDelay(20))
}

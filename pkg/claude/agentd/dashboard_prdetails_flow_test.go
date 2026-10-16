package agentd_test

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agent"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/testharness"
)

type prDetailsResp struct {
	URL     string `json:"url"`
	Details *struct {
		Title        string `json:"title"`
		State        string `json:"state"`
		Author       string `json:"author"`
		HeadRef      string `json:"head_ref"`
		BaseRef      string `json:"base_ref"`
		Excerpt      string `json:"excerpt"`
		Additions    int    `json:"additions"`
		Deletions    int    `json:"deletions"`
		ChangedFiles int    `json:"changed_files"`
		Commits      int    `json:"commits"`
		CreatedAt    string `json:"created_at"`
		MergedAt     string `json:"merged_at"`
	} `json:"details"`
	Resolved   bool `json:"resolved"`
	Refreshing bool `json:"refreshing"`
}

func fetchPRDetails(t *testing.T, mux http.Handler, prURL string) prDetailsResp {
	t.Helper()
	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet,
		"/api/pr-details?url="+url.QueryEscape(prURL), nil))
	require.Equalf(t, http.StatusOK, rec.Code, "GET /api/pr-details: %s", rec.Body.String())
	var out prDetailsResp
	testharness.DecodeJSON(t, rec, &out)
	return out
}

// Scenario: the operator hovers a PR link in the Groups tab. The first hover
// finds nothing cached and schedules the gh read; the panel's next poll
// serves the card, with the description reduced to its opening prose.
func TestDashboardPRDetails_HoverResolvesCard(t *testing.T) {
	const prURL = "https://github.com/acme/app/pull/88"
	_ = newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	savePresentedPRPolicy(t, "github.com/acme")
	calls := 0
	t.Cleanup(agentd.SetPRDetailsFetcherForTest(func(rawURL string) (string, bool) {
		calls++
		assert.Equal(t, prURL, rawURL)
		return `{"title":"Dock: remember collapsed sections","state":"MERGED","isDraft":false,
			"author":{"login":"alice"},"createdAt":"2026-10-01T10:00:00Z","mergedAt":"2026-10-02T12:00:00Z",
			"closedAt":"2026-10-02T12:00:00Z","additions":412,"deletions":88,"changedFiles":9,
			"commits":[{"oid":"a"},{"oid":"b"},{"oid":"c"}],"headRefName":"dock-collapse","baseRefName":"main",
			"body":"## Background / Purpose\n\nThe dock forgot **collapsed** sections on reload.\n\n## Changes\n- Persist [state](https://x/y) per tab"}`, true
	}))
	mux := agentd.BuildDashboardHandlerForTest()

	first := fetchPRDetails(t, mux, prURL)
	assert.False(t, first.Resolved)
	assert.Nil(t, first.Details)
	assert.True(t, first.Refreshing, "the first hover must schedule the read")
	agentd.WaitForBackgroundForTest()

	second := fetchPRDetails(t, mux, prURL)
	require.True(t, second.Resolved)
	require.NotNil(t, second.Details)
	d := second.Details
	assert.Equal(t, "Dock: remember collapsed sections", d.Title)
	assert.Equal(t, "merged", d.State)
	assert.Equal(t, "alice", d.Author)
	assert.Equal(t, "dock-collapse", d.HeadRef)
	assert.Equal(t, "main", d.BaseRef)
	assert.Equal(t, 412, d.Additions)
	assert.Equal(t, 88, d.Deletions)
	assert.Equal(t, 9, d.ChangedFiles)
	assert.Equal(t, 3, d.Commits)
	assert.Equal(t, "2026-10-02T12:00:00Z", d.MergedAt)
	assert.Equal(t, "The dock forgot collapsed sections on reload. Persist state per tab", d.Excerpt,
		"headings are skipped and markdown is stripped")
	assert.False(t, second.Refreshing, "a fresh card spends no gh call")
	assert.Equal(t, 1, calls)
}

func TestDashboardPRDetails_RejectsPROutsideRemotePolicy(t *testing.T) {
	const prURL = "https://github.com/victim/private/pull/1"
	_ = newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	savePresentedPRPolicy(t, "github.com/tofutools")
	called := false
	t.Cleanup(agentd.SetPRDetailsFetcherForTest(func(string) (string, bool) {
		called = true
		return "", false
	}))
	mux := agentd.BuildDashboardHandlerForTest()

	resp := fetchPRDetails(t, mux, prURL)
	assert.False(t, resp.Refreshing)
	agentd.WaitForBackgroundForTest()
	assert.False(t, called, "hover must not spend credentials outside remote policy")

	rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet,
		"/api/pr-details?url="+url.QueryEscape("https://example.com/not-a-pr"), nil))
	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// A failed gh read (logged out, PR gone, rate limit) must not turn every poll
// of an open card into another subprocess: the failure holds off retries, and
// the response stops promising a newer answer so the panel stops polling.
func TestDashboardPRDetails_FailedReadCoolsDown(t *testing.T) {
	const prURL = "https://github.com/acme/app/pull/404"
	_ = newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	savePresentedPRPolicy(t, "github.com/acme")
	calls := 0
	t.Cleanup(agentd.SetPRDetailsFetcherForTest(func(string) (string, bool) {
		calls++
		return "", false
	}))
	mux := agentd.BuildDashboardHandlerForTest()

	first := fetchPRDetails(t, mux, prURL)
	assert.True(t, first.Refreshing)
	agentd.WaitForBackgroundForTest()
	for range 3 {
		again := fetchPRDetails(t, mux, prURL)
		assert.False(t, again.Resolved)
		assert.False(t, again.Refreshing, "nothing is in flight during the cooldown")
	}
	agentd.WaitForBackgroundForTest()
	assert.Equal(t, 1, calls, "one failed read, not one per poll")
}

// Scenario: an agent sits on a branch with a PR. The branch lookup that runs
// anyway also reads the PR's title, and the snapshot carries it on the PR link
// so the hover card can name the PR before any details are fetched — with no
// hover and no extra gh call.
func TestDashboardPRDetails_BranchLookupTitleReachesSnapshot(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	const (
		conv   = "prtitles-0000-0000-0000-000000000001"
		branch = "feature-pr-title"
		prURL  = "https://github.com/acme/app/pull/91"
	)
	t.Cleanup(agentd.SetGitInfoResolverForTest(
		func(_, b string) (string, string, int, string, string, bool) {
			if b != branch {
				return "", "", 0, "", "", false
			}
			return "https://github.com/acme/app", "main", 91, prURL, "open", true
		}))
	t.Cleanup(agentd.SetBranchPRTitleForTest(func(string) string { return "Add the PR title to links" }))
	t.Cleanup(agentd.SetPRDetailsFetcherForTest(func(string) (string, bool) {
		t.Error("no hover happened, so no details read may run")
		return "", false
	}))

	f := newFlow(t)
	f.HaveGroup("squad")
	f.HaveAliveSessionOnBranch(conv, "spwn-prtitle", "tmux-prtitle", f.TestCwd("wt/title"), branch)
	f.HaveMember("squad", conv)
	require.NotNil(t, agent.FreshConvRowResolved(conv), "conv_index scan")
	mux := agentd.BuildDashboardHandlerForTest()

	_ = fetchDashSnapshot(t, mux)
	agentd.WaitForBackgroundForTest()
	snap := fetchDashSnapshot(t, mux)
	member := findDashMember(snap, "squad", conv)
	require.NotNil(t, member)
	require.Equal(t, prURL, member.BranchPRURL)
	assert.Equal(t, "Add the PR title to links", member.BranchPRTitle)
	warm := findAgent(snap.Agents, conv)
	require.NotNil(t, warm)
	assert.Equal(t, "Add the PR title to links", warm.BranchPRTitle)
}

package agentd

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const reviewedPRURL = "https://github.com/acme/app/pull/12"

func pollDecodedAuthoredSearch(t *testing.T, payload string) {
	t.Helper()
	previous := authoredOpenPRResolver
	t.Cleanup(func() { authoredOpenPRResolver = previous })
	authoredOpenPRResolver = func() (dashboardAuthoredOpenPRs, error) {
		view, _, err := decodeAuthoredOpenPRGraphQL([]byte(payload), "octocat")
		return view, err
	}
	require.NoError(t, pollAuthoredOpenPRs())
}

func TestAuthoredSearchReviewsReachTheSnapshotAndSurviveDecisionOnlyTicks(t *testing.T) {
	setupTestDB(t)
	setAuthoredOpenPRActiveLogin("")

	// A review tick: the reviewer list is present. CodeRabbit's approval is a
	// bot's and must not make the PR look approved by a human.
	pollDecodedAuthoredSearch(t, `{"data":{"search":{"issueCount":1,"nodes":[
	  {"number":12,"title":"Ship it","url":"`+reviewedPRURL+`","reviewDecision":null,
	   "latestOpinionatedReviews":{"nodes":[
	     {"state":"APPROVED","url":"`+reviewedPRURL+`#pullrequestreview-1","submittedAt":"2026-10-15T10:00:00Z","author":{"__typename":"User","login":"alice"}},
	     {"state":"CHANGES_REQUESTED","submittedAt":"2026-10-15T09:00:00Z","author":{"__typename":"User","login":"bob"}},
	     {"state":"APPROVED","author":{"__typename":"Bot","login":"coderabbitai"}},
	     {"state":"COMMENTED","author":{"__typename":"User","login":"carol"}}
	   ]}}]}}}`)

	idx := prReviewsIndexFor([]string{reviewedPRURL + "/files"})
	got := idx[prStateKey(reviewedPRURL)]
	require.NotNil(t, got)
	assert.Equal(t, "", got.Decision, "no required reviews on this repository")
	assert.Equal(t, 1, got.Approved, "the bot approval is listed, not counted")
	assert.Equal(t, 1, got.ChangesRequested)
	assert.True(t, got.ReviewersKnown)
	require.Len(t, got.Reviewers, 3, "a comment-only review carries no opinion")
	assert.Equal(t, "bob", got.Reviewers[0].Login, "blocking reviews sort first")
	assert.True(t, got.Reviewers[2].Bot)

	// A decision-only tick (no reviewer list selected) keeps the list and
	// takes the fresh decision.
	pollDecodedAuthoredSearch(t, `{"data":{"search":{"issueCount":1,"nodes":[
	  {"number":12,"title":"Ship it","url":"`+reviewedPRURL+`","reviewDecision":"CHANGES_REQUESTED"}]}}}`)
	got = prReviewsIndexFor([]string{reviewedPRURL})[prStateKey(reviewedPRURL)]
	require.NotNil(t, got)
	assert.Equal(t, "changes_requested", got.Decision)
	assert.Len(t, got.Reviewers, 3)
	assert.Equal(t, 1, got.Approved)

	view := withAuthoredPRReviews(loadAuthoredOpenPRsSnapshot(nil), prReviewsIndexFor([]string{reviewedPRURL}))
	require.Len(t, view.Items, 1)
	require.NotNil(t, view.Items[0].Reviews, "the footer list carries the same review summary as Groups")
	assert.Equal(t, "changes_requested", view.Items[0].Reviews.Decision)

	links := repoLinksView{BranchPRURL: reviewedPRURL, PresentedPRs: []presentedPRView{{URL: reviewedPRURL + "/"}}}.
		withPRReviews(prReviewsIndexFor([]string{reviewedPRURL}))
	require.NotNil(t, links.BranchReviews)
	require.NotNil(t, links.PresentedPRs[0].Reviews)
}

func TestAuthoredPRSearchAddsReviewersOnlyWhenAsked(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	for _, window := range []int{0, 3} {
		plain := strings.Join(authoredPRSearchArgs("octocat", window, now, false), "\n")
		assert.Contains(t, plain, "reviewDecision", "the free scalar rides every tick")
		assert.NotContains(t, plain, "latestOpinionatedReviews")

		reviews := strings.Join(authoredPRSearchArgs("octocat", window, now, true), "\n")
		assert.Equal(t, 1, strings.Count(reviews, "latestOpinionatedReviews"),
			"only the open search pays for reviewer lists")
	}
}

func TestAuthoredOpenPRReviewsDue(t *testing.T) {
	previous := authoredOpenPRReviewsFetchedAt.Load()
	t.Cleanup(func() { authoredOpenPRReviewsFetchedAt.Store(previous) })
	now := time.Now()
	authoredOpenPRReviewsFetchedAt.Store(0)
	assert.True(t, authoredOpenPRReviewsDue(now), "the first poll fetches reviewers")
	authoredOpenPRReviewsFetchedAt.Store(now.Add(-10 * time.Second).UnixNano())
	assert.False(t, authoredOpenPRReviewsDue(now))
	authoredOpenPRReviewsFetchedAt.Store(now.Add(-authoredOpenPRReviewInterval).UnixNano())
	assert.True(t, authoredOpenPRReviewsDue(now))
}

func TestGHReviewListDoesNotOverrideAFreshSearchList(t *testing.T) {
	now := time.Now()
	search := mergePRReviewObservation(nil, prReviewObservation{
		ReviewersFetched: true, Source: prReviewSourceSearch,
		Reviewers: []prReviewer{{Login: "alice", State: "approved"}},
	}, now)
	require.Equal(t, 1, search.Approved)

	// gh's latestReviews can show alice's later COMMENT instead of her
	// approval; while the search list is fresh, gh only updates the decision.
	gh := prReviewObservationFromGH("APPROVED", []ghLatestReview{{State: "COMMENTED"}})
	merged := mergePRReviewObservation(&search, *gh, now.Add(30*time.Second))
	assert.Equal(t, "approved", merged.Decision)
	assert.Equal(t, 1, merged.Approved)
	assert.Equal(t, prReviewSourceSearch, merged.ReviewersSource)

	// Once the search stops covering the PR, gh's list is used.
	merged = mergePRReviewObservation(&search, *gh, now.Add(prReviewsSearchListAuthority+time.Second))
	assert.Equal(t, 0, merged.Approved)
	assert.Equal(t, prReviewSourceGH, merged.ReviewersSource)
	assert.True(t, merged.ReviewersKnown)
}

func TestNormalizePRReviewerConstrainsDisplayedValues(t *testing.T) {
	_, ok := normalizePRReviewer(`<img src=x>`, "APPROVED", "", "", false)
	assert.False(t, ok, "a login outside GitHub's charset is dropped")
	_, ok = normalizePRReviewer("alice", "PENDING", "", "", false)
	assert.False(t, ok)

	r, ok := normalizePRReviewer("dependabot[bot]", "approved", "javascript:alert(1)", "not-a-time", false)
	require.True(t, ok)
	assert.True(t, r.Bot, "a [bot] login is a bot even without GraphQL's typename")
	assert.Empty(t, r.URL, "only github.com review links become hrefs")
	assert.Empty(t, r.SubmittedAt)
	assert.Equal(t, "approved", r.State)
}

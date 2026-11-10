package agentd

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// prreviews_gitlab_test.go: a merge request's approvals and reviewer states as
// review rows, and the approvals-only fallback for an instance without
// reviewer states.

func TestGLReviewers_ApprovalsAndChangeRequests(t *testing.T) {
	state := func(s string) *struct {
		ReviewState string `json:"reviewState"`
	} {
		return &struct {
			ReviewState string `json:"reviewState"`
		}{ReviewState: s}
	}
	approvedBy := []glReviewUser{
		{Username: "ann"},
		{Username: "project_12_bot_3f9a"},
		{Username: "evil\"><script>"},
	}
	reviewers := []glReviewUser{
		{Username: "ann", MergeRequestInteraction: state("APPROVED")},
		{Username: "martin", MergeRequestInteraction: state("REQUESTED_CHANGES")},
		{Username: "carol", MergeRequestInteraction: state("REVIEWED")},
		{Username: "dave", MergeRequestInteraction: state("UNREVIEWED")},
		{Username: "renovate", Bot: true, MergeRequestInteraction: state("APPROVED")},
		{Username: "erin"},
	}
	got := glReviewers(testMRBase+"/", approvedBy, reviewers)
	assert.Equal(t, []prReviewer{
		{Login: "ann", State: "approved", URL: testMRBase + "/ann"},
		{Login: "project_12_bot_3f9a", State: "approved", Bot: true, URL: testMRBase + "/project_12_bot_3f9a"},
		{Login: "martin", State: "changes_requested", URL: testMRBase + "/martin"},
		{Login: "renovate", State: "approved", Bot: true, URL: testMRBase + "/renovate"},
	}, got, "approvers once each, then change requests; comments and unstarted reviews carry no opinion")

	summary := mergePRReviewObservation(nil, prReviewObservation{Decision: glReviewDecision("REQUESTED_CHANGES"),
		Reviewers: got, ReviewersFetched: true}, time.Now())
	assert.Equal(t, "changes_requested", summary.Decision)
	assert.Equal(t, 1, summary.Approved, "bots are listed but never counted")
	assert.Equal(t, 1, summary.ChangesRequested)
}

func TestGLReviewDecision(t *testing.T) {
	for status, want := range map[string]string{
		"NOT_APPROVED": "review_required", "requested_changes": "changes_requested",
		"MERGEABLE": "", "CONFLICT": "", "": "",
	} {
		assert.Equal(t, want, glReviewDecision(status), status)
	}
}

// glReviewsTransport answers the two review queries: the reviewer one with
// rich, or with an unknown-field error when rich is empty; the approvals one
// with approvals.
func glReviewsTransport(t *testing.T, rich, approvals string) (*[]string, func()) {
	t.Helper()
	var mu sync.Mutex
	var sent []string
	restore := SetGitLabTransportForTest(func(_ context.Context, _, _ string, req glAPIRequest) (glAPIResult, error) {
		require.True(t, req.GraphQL)
		query := req.Body.(map[string]any)["query"].(string)
		vars := req.Body.(map[string]any)["variables"].(map[string]any)
		assert.Equal(t, "contoso/sub/player", vars["project"], "the project travels as a variable")
		assert.Equal(t, "7", vars["iid"])
		mu.Lock()
		defer mu.Unlock()
		if strings.Contains(query, "mergeRequestInteraction") {
			sent = append(sent, "reviewers")
			if rich == "" {
				return glAPIResult{Status: http.StatusOK, Header: http.Header{}, Body: []byte(`{"errors":[{
					"message":"Field 'reviewState' doesn't exist on type 'MergeRequestInteraction'",
					"extensions":{"code":"undefinedField","typeName":"MergeRequestInteraction","fieldName":"reviewState"}}]}`)}, nil
			}
			return glAPIResult{Status: http.StatusOK, Header: http.Header{}, Body: []byte(rich)}, nil
		}
		sent = append(sent, "approvals")
		return glAPIResult{Status: http.StatusOK, Header: http.Header{}, Body: []byte(approvals)}, nil
	})
	return &sent, func() {
		restore()
		glReviewStatesMissing.Clear()
	}
}

var testMRRef = gitlabMRRef{base: testMRBase, host: "gitlab.example.test", project: "contoso/sub/player", number: 7}

func TestGitLabMRReviews_ReadsReviewerStates(t *testing.T) {
	sent, restore := glReviewsTransport(t, `{"data":{"project":{"mergeRequest":{"detailedMergeStatus":"REQUESTED_CHANGES",
		"approvedBy":{"nodes":[{"username":"ann","bot":false}]},
		"reviewers":{"nodes":[{"username":"martin","bot":false,"mergeRequestInteraction":{"reviewState":"REQUESTED_CHANGES"}}]}}}}}`, "")
	defer restore()

	obs := gitlabMRReviews(context.Background(), testMRBase, "tok", testMRRef)
	require.NotNil(t, obs)
	assert.Equal(t, "changes_requested", obs.Decision)
	assert.True(t, obs.ReviewersFetched)
	require.Len(t, obs.Reviewers, 2)
	assert.Equal(t, "changes_requested", obs.Reviewers[1].State)
	assert.Equal(t, []string{"reviewers"}, *sent)
}

// TestGitLabMRReviews_AnOldInstanceFallsBackToApprovals: an instance that does
// not know reviewer states answers with approvals only, and is not asked for
// them again.
func TestGitLabMRReviews_AnOldInstanceFallsBackToApprovals(t *testing.T) {
	sent, restore := glReviewsTransport(t, "", `{"data":{"project":{"mergeRequest":{"detailedMergeStatus":"NOT_APPROVED",
		"approvedBy":{"nodes":[{"username":"ann"}]}}}}}`)
	defer restore()

	obs := gitlabMRReviews(context.Background(), testMRBase, "tok", testMRRef)
	require.NotNil(t, obs)
	assert.Equal(t, "review_required", obs.Decision)
	assert.Equal(t, []prReviewer{{Login: "ann", State: "approved", URL: testMRBase + "/ann"}}, obs.Reviewers)
	assert.Equal(t, []string{"reviewers", "approvals"}, *sent)

	obs = gitlabMRReviews(context.Background(), testMRBase, "tok", testMRRef)
	require.NotNil(t, obs)
	assert.Equal(t, []string{"reviewers", "approvals", "approvals"}, *sent, "the missing fields are remembered")
}

func TestGitLabMRReviews_AFailedReadKeepsTheCache(t *testing.T) {
	restore := SetGitLabTransportForTest(func(context.Context, string, string, glAPIRequest) (glAPIResult, error) {
		return glAPIResult{Status: http.StatusBadGateway, Header: http.Header{}, Body: []byte(`{"message":"bad gateway"}`)}, nil
	})
	defer restore()
	assert.Nil(t, gitlabMRReviews(context.Background(), testMRBase, "tok", testMRRef),
		"nil, so the cached reviews stand")
	assert.False(t, glReviewStatesKnownMissing(testMRBase), "a failure is not a missing field")
}

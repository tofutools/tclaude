package agentd

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// openmrs_backoff_test.go: the Open MRs poller's two GraphQL side reads (the
// reviewers and the recently closed list) when they fail. A 429 is waited
// out as the list reads' are, a failing recently closed read waits as long
// as a good one, and a failed reviewer read never costs a row its approvals.

// sideReadWorld answers the list query with one merge request approved by
// ann, and the reviewer and recent reads with what reviewers and recent give.
// It counts the reads of each kind.
func sideReadWorld(t *testing.T, reviewers, recent func() glAPIResult) map[string]int {
	t.Helper()
	setupTestDB(t)
	tokenPath := filepath.Join(t.TempDir(), "gitlab-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("glpat-open-mrs\n"), 0o600))
	days := 7
	require.NoError(t, config.Save(&config.Config{
		Dashboard: &config.DashboardConfig{RecentPRWindowDays: &days},
		Agent: &config.AgentConfig{GitProxy: &config.GitProxyConfig{
			AllowedRemotes:  []string{"gitlab.example.test"},
			GitLabHosts:     map[string]string{"gitlab.example.test": testMRBase},
			GitLabTokenFile: tokenPath,
		}},
	}))
	t.Cleanup(func() { resetAuthoredOpenMRs("") })
	t.Cleanup(glRecentMRsMissing.Clear)
	t.Cleanup(glReviewStatesMissing.Clear)
	var mu sync.Mutex
	counts := map[string]int{}
	t.Cleanup(SetGitLabTransportForTest(func(_ context.Context, _, _ string, req glAPIRequest) (glAPIResult, error) {
		answer := func() glAPIResult {
			if !req.GraphQL {
				return glAPIResult{Status: http.StatusOK, Body: []byte("[]")}
			}
			query := req.Body.(map[string]any)["query"].(string)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case strings.Contains(query, "mergedAt"):
				counts["recent"]++
				return recent()
			case strings.Contains(query, "mergeRequestInteraction"):
				counts["reviewers"]++
				return reviewers()
			case strings.Contains(query, "authoredMergeRequests"):
				return glAPIResult{Status: http.StatusOK, Body: []byte(`{"data":{"currentUser":{"username":"jdoe",
					"authoredMergeRequests":{"count":1,"nodes":[{"iid":"7","title":"Change 7",
					"webUrl":"` + testMRBase + `/acme/sub/app/-/merge_requests/7","updatedAt":"2026-11-02T10:00:00Z",
					"detailedMergeStatus":"MERGEABLE","headPipeline":{"status":"SUCCESS"},
					"approvedBy":{"nodes":[{"username":"ann"}]}}]}}}}`)}
			}
			return glAPIResult{Status: http.StatusOK, Body: []byte(`{"data":{}}`)}
		}()
		if answer.Header == nil {
			answer.Header = http.Header{}
		}
		return answer, nil
	}))
	return counts
}

func okResult(body string) func() glAPIResult {
	return func() glAPIResult { return glAPIResult{Status: http.StatusOK, Body: []byte(body)} }
}

func rateLimited(seconds string) func() glAPIResult {
	return func() glAPIResult {
		return glAPIResult{Status: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{seconds}},
			Body: []byte(`{"message":"429 Too Many Requests"}`)}
	}
}

func badGateway() glAPIResult {
	return glAPIResult{Status: http.StatusBadGateway, Body: []byte(`{"message":"bad gateway"}`)}
}

const emptyRecent = `{"data":{"currentUser":{"merged":{"count":0,"nodes":[]},"closed":{"count":0,"nodes":[]}}}}`
const emptyReviewers = `{"data":{"currentUser":{"authoredMergeRequests":{"nodes":[]}}}}`

// TestOpenMRs_ARateLimitedSideReadIsWaitedOut: a 429 on either GraphQL side
// read makes the poll wait what GitLab asked, as a 429 on a list read does.
func TestOpenMRs_ARateLimitedSideReadIsWaitedOut(t *testing.T) {
	t.Run("the reviewer read", func(t *testing.T) {
		sideReadWorld(t, rateLimited("240"), okResult(emptyRecent))
		retry, err := pollAuthoredOpenMRs(context.Background())
		require.NoError(t, err, "the list still answered")
		assert.Equal(t, 240*time.Second, retry)
		delay, failures := authoredOpenMRNextDelay(retry, err, 0)
		assert.Equal(t, 240*time.Second, delay, "the next poll waits it out")
		assert.Zero(t, failures, "a rate limit is not a failure")
	})
	t.Run("the recently closed read", func(t *testing.T) {
		sideReadWorld(t, okResult(emptyReviewers), rateLimited("180"))
		retry, err := pollAuthoredOpenMRs(context.Background())
		require.NoError(t, err)
		assert.Equal(t, 180*time.Second, retry)
	})
}

// TestOpenMRs_AFailingRecentReadWaitsAsLongAsAGoodOne: a recently closed read
// that keeps failing is asked for every authoredRecentMREvery polls, not on
// every poll until it first succeeds.
func TestOpenMRs_AFailingRecentReadWaitsAsLongAsAGoodOne(t *testing.T) {
	counts := sideReadWorld(t, okResult(emptyReviewers), badGateway)
	for range 6 {
		_, err := pollAuthoredOpenMRs(context.Background())
		require.NoError(t, err)
	}
	assert.Equal(t, 2, counts["recent"], "polls 1 and 6")
	view := authoredOpenMRsSnapshot(loadCfg(t), nil)
	require.NotNil(t, view)
	assert.Zero(t, view.RecentWindowDays, "no list read yet, so no Closed filter")
}

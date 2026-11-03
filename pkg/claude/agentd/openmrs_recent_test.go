package agentd

import (
	"context"
	"encoding/json"
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

// openmrs_recent_test.go: the Open MRs recently closed list. The poller reads
// it every authoredRecentMREvery polls, in the window Open PRs uses, and the
// snapshot carries it as the Open PRs list carries its own.

// recentMRsWorld configures one instance with a window of windowDays and
// answers the recent query with recent (or an unknown-field error when recent
// is ""). It returns the variables of every recent query sent.
func recentMRsWorld(t *testing.T, windowDays int, recent string) *[]map[string]any {
	t.Helper()
	setupTestDB(t)
	tokenPath := filepath.Join(t.TempDir(), "gitlab-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("glpat-open-mrs\n"), 0o600))
	require.NoError(t, config.Save(&config.Config{
		Dashboard: &config.DashboardConfig{RecentPRWindowDays: &windowDays},
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
	var sent []map[string]any
	t.Cleanup(SetGitLabTransportForTest(func(_ context.Context, _, _ string, req glAPIRequest) (glAPIResult, error) {
		ok := func(body string) (glAPIResult, error) {
			return glAPIResult{Status: http.StatusOK, Header: http.Header{}, Body: []byte(body)}, nil
		}
		if !req.GraphQL {
			return ok("[]")
		}
		body := req.Body.(map[string]any)
		query := body["query"].(string)
		switch {
		case strings.Contains(query, "mergedAt"):
			mu.Lock()
			sent = append(sent, body["variables"].(map[string]any))
			mu.Unlock()
			if recent == "" {
				return ok(`{"errors":[{"message":"Field 'authoredMergeRequests' doesn't accept argument 'updatedAfter'",
					"extensions":{"code":"argumentNotAccepted"}}]}`)
			}
			return ok(recent)
		case strings.Contains(query, "mergeRequestInteraction"):
			return ok(`{"data":{"currentUser":{"authoredMergeRequests":{"nodes":[]}}}}`)
		default:
			return ok(`{"data":{"currentUser":{"username":"jdoe","authoredMergeRequests":{"count":0,"nodes":[]}}}}`)
		}
	}))
	return &sent
}

func recentMRsAnswer(now time.Time) string {
	at := func(d time.Duration) string { return now.Add(-d).UTC().Format(time.RFC3339) }
	doc := map[string]any{"data": map[string]any{"currentUser": map[string]any{
		"merged": map[string]any{"count": 2, "nodes": []map[string]any{
			{"iid": "5", "title": "Landed", "webUrl": testMRBase + "/acme/sub/app/-/merge_requests/5",
				"updatedAt": at(time.Hour), "mergedAt": at(2 * time.Hour), "headPipeline": map[string]any{"status": "FAILED"}},
			{"iid": "4", "title": "Landed long ago, commented on today", "webUrl": testMRBase + "/acme/sub/app/-/merge_requests/4",
				"updatedAt": at(time.Hour), "mergedAt": at(10 * 24 * time.Hour)},
		}},
		"closed": map[string]any{"count": 1, "nodes": []map[string]any{
			{"iid": "6", "title": "Abandoned", "webUrl": testMRBase + "/acme/sub/app/-/merge_requests/6",
				"updatedAt": at(30 * time.Minute), "closedAt": at(30 * time.Minute)},
		}},
	}}}
	raw, _ := json.Marshal(doc)
	return string(raw)
}

func TestOpenMRs_RecentlyClosedFillsTheSnapshotsRecentList(t *testing.T) {
	now := time.Now()
	sent := recentMRsWorld(t, 7, recentMRsAnswer(now))
	_, err := pollAuthoredOpenMRs(context.Background())
	require.NoError(t, err)

	require.Len(t, *sent, 1)
	vars := (*sent)[0]
	assert.EqualValues(t, authoredRecentMRLimit, vars["first"])
	after, err := time.Parse(time.RFC3339, vars["after"].(string))
	require.NoError(t, err)
	assert.WithinDuration(t, now.Add(-7*24*time.Hour), after, time.Minute, "the window travels as a variable")

	view := authoredOpenMRsSnapshot(loadCfg(t), nil)
	require.NotNil(t, view)
	assert.Equal(t, 7, view.RecentWindowDays)
	require.Len(t, view.Recent, 2, "a merge request merged before the window is left out, however recently updated")
	assert.Equal(t, 6, view.Recent[0].Number, "newest first")
	assert.Equal(t, "closed", view.Recent[0].State)
	assert.Equal(t, 5, view.Recent[1].Number)
	assert.Equal(t, "merged", view.Recent[1].State)
	assert.Equal(t, now.Add(-2*time.Hour).UTC().Format(time.RFC3339), view.Recent[1].ClosedAt)
	require.NotNil(t, view.Recent[1].Checks)
	assert.Equal(t, "failing", view.Recent[1].Checks.State, "the last pipeline status stays with the row")
	assert.Nil(t, view.Recent[1].Reviews, "history carries no review pill")
	assert.Empty(t, view.Items, "the open list stays about open work")
}

// TestOpenMRs_RecentlyClosedIsReadEveryFifthPoll: a merged merge request is
// history; it costs one read every authoredRecentMREvery polls, and at once
// when the window changes.
func TestOpenMRs_RecentlyClosedIsReadEveryFifthPoll(t *testing.T) {
	sent := recentMRsWorld(t, 7, recentMRsAnswer(time.Now()))
	for range 6 {
		_, err := pollAuthoredOpenMRs(context.Background())
		require.NoError(t, err)
	}
	assert.Len(t, *sent, 2, "polls 1 and 6")

	days := 14
	require.NoError(t, config.Save(&config.Config{
		Dashboard: &config.DashboardConfig{RecentPRWindowDays: &days},
		Agent:     loadCfg(t).Agent,
	}))
	_, err := pollAuthoredOpenMRs(context.Background())
	require.NoError(t, err)
	assert.Len(t, *sent, 3, "a changed window is read at once")
}

func TestOpenMRs_RecentlyClosedOffOrUnsupportedHasNoList(t *testing.T) {
	t.Run("a window of 0 asks nothing", func(t *testing.T) {
		sent := recentMRsWorld(t, 0, recentMRsAnswer(time.Now()))
		_, err := pollAuthoredOpenMRs(context.Background())
		require.NoError(t, err)
		assert.Empty(t, *sent)
		view := authoredOpenMRsSnapshot(loadCfg(t), nil)
		require.NotNil(t, view)
		assert.Zero(t, view.RecentWindowDays, "no Closed filter")
		assert.Empty(t, view.Recent)
	})
	t.Run("an instance without the fields is asked once", func(t *testing.T) {
		sent := recentMRsWorld(t, 7, "")
		for range 6 {
			_, err := pollAuthoredOpenMRs(context.Background())
			require.NoError(t, err, "a missing field is not a failed poll")
		}
		assert.Len(t, *sent, 1)
		view := authoredOpenMRsSnapshot(loadCfg(t), nil)
		require.NotNil(t, view)
		assert.Zero(t, view.RecentWindowDays, "no list, so no Closed filter rather than an empty one")
	})
}

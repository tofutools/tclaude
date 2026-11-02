package agentd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// prs_gitlab_jobs_test.go: the dashboard's CI summary for a merge request
// covers every page of the head pipeline's jobs, and an incomplete read
// never reads as a green pipeline.

const jobsTestBase = "https://gitlab.yamzz.test:6080"

func jobsPage(t *testing.T, from, n int, status string) []byte {
	t.Helper()
	jobs := make([]map[string]any, n)
	for i := range jobs {
		jobs[i] = map[string]any{"id": from + i, "name": "job-" + strconv.Itoa(from+i), "stage": "test", "status": status}
	}
	body, err := json.Marshal(jobs)
	require.NoError(t, err)
	return body
}

// nextLink is the Link header GitLab sends for another page on the instance.
func nextLink(page int) http.Header {
	return http.Header{"Link": []string{fmt.Sprintf(
		`<%s/api/v4/projects/yamzz%%2Fapp/pipelines/99/jobs?per_page=100&page=%d>; rel="next"`, jobsTestBase, page)}}
}

func jobsRef(t *testing.T) gitlabMRRef {
	t.Helper()
	ref, ok := gitlabMRRefFromURL(jobsTestBase + "/yamzz/app/-/merge_requests/7")
	require.True(t, ok)
	return ref
}

func TestGitLabPipelineChecks_ReadsEveryPageOfJobs(t *testing.T) {
	calls := 0
	t.Cleanup(SetGitLabTransportForTest(func(_ context.Context, base, _ string, req glAPIRequest) (glAPIResult, error) {
		calls++
		require.Equal(t, jobsTestBase, base)
		if calls == 1 {
			return glAPIResult{Status: http.StatusOK, Body: jobsPage(t, 1, 100, "success"), Header: nextLink(2)}, nil
		}
		return glAPIResult{Status: http.StatusOK, Body: jobsPage(t, 101, 1, "failed"), Header: http.Header{}}, nil
	}))
	info := gitlabPipelineChecks(context.Background(), jobsTestBase, "glpat", jobsRef(t),
		&glPipeline{ID: 99, Status: "failed", WebURL: jobsTestBase + "/yamzz/app/-/pipelines/99"}, time.Now())
	assert.Equal(t, 2, calls, "the second page is read")
	assert.Equal(t, "failing", info.Summary.State, "a failure on page two turns the badge red")
	assert.Equal(t, 101, info.Summary.Total)
	assert.Equal(t, 1, info.Summary.Failed)
}

func TestGitLabPipelineChecks_IncompleteReadTakesThePipelinesVerdict(t *testing.T) {
	run := func(t *testing.T, pipelineStatus string, failPage int) prChecksInfo {
		t.Helper()
		page := 0
		t.Cleanup(SetGitLabTransportForTest(func(_ context.Context, _, _ string, req glAPIRequest) (glAPIResult, error) {
			page++
			if page == failPage {
				return glAPIResult{Status: http.StatusBadGateway, Body: []byte(`{"message":"bad gateway"}`), Header: http.Header{}}, nil
			}
			return glAPIResult{Status: http.StatusOK, Body: jobsPage(t, page*100, 100, "success"), Header: nextLink(page + 1)}, nil
		}))
		return gitlabPipelineChecks(context.Background(), jobsTestBase, "glpat", jobsRef(t),
			&glPipeline{ID: 99, Status: pipelineStatus}, time.Now())
	}

	t.Run("the page cap: green so far, pipeline still running", func(t *testing.T) {
		info := run(t, "running", 0)
		assert.Equal(t, "pending", info.Summary.State, "a capped, all-green subset is not a passing pipeline")
		assert.Equal(t, maxGLPaginatedPages*100, info.Summary.Total, "the summary counts every job read")
		assert.Len(t, info.Checks, prChecksMaxRuns, "only the displayed list is capped")
	})
	t.Run("the page cap: pipeline failed", func(t *testing.T) {
		assert.Equal(t, "failing", run(t, "failed", 0).Summary.State)
	})
	t.Run("the page cap: pipeline passed", func(t *testing.T) {
		assert.Equal(t, "passing", run(t, "success", 0).Summary.State, "GitLab's own verdict is green")
	})
	t.Run("a page that fails mid-read", func(t *testing.T) {
		info := run(t, "running", 2)
		assert.Equal(t, "pending", info.Summary.State)
		assert.Equal(t, 100, info.Summary.Total)
	})
}

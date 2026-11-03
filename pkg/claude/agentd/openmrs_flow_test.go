package agentd_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
)

// openmrs_flow_test.go: the footer's Open MRs list end to end. One poll
// against the stubbed GitLab transport, read back through the real dashboard
// snapshot handler.

const glOpenMRsAnswer = `{"data":{"currentUser":{"username":"jdoe","authoredMergeRequests":{"count":2,"nodes":[
{"iid":"7","title":"Add a thing","webUrl":"https://gitlab.contoso.test:8929/contoso/sub/player/-/merge_requests/7",
 "draft":false,"updatedAt":"2026-11-02T10:00:00Z","conflicts":false,"detailedMergeStatus":"MERGEABLE",
 "headPipeline":{"status":"SUCCESS"},"approvedBy":{"nodes":[{"username":"ann"}]}},
{"iid":"8","title":"Draft: Try a thing","webUrl":"https://gitlab.contoso.test:8929/contoso/sub/player/-/merge_requests/8",
 "draft":true,"updatedAt":"2026-11-02T11:00:00Z","conflicts":true,"detailedMergeStatus":"CONFLICT",
 "headPipeline":{"status":"RUNNING"},"approvedBy":{"nodes":[]}}
]}}}}`

func TestOpenMRs_SnapshotCarriesTheTokenUsersMergeRequests(t *testing.T) {
	// The dashboard handler checks Origin against the popup base. Set it here
	// rather than relying on an earlier test leaving the global set, which
	// holds in a full package run but not under -run.
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_, _, gl := glWorld(t, []string{"gitlab.contoso.test/contoso"})
	t.Cleanup(agentd.ResetAuthoredOpenMRsForTest)
	gl.route = func(req agentd.GitLabRequestForTest) (glCanned, bool) {
		switch {
		case !req.GraphQL && req.Path == "merge_requests":
			return glCanned{Status: http.StatusOK, Body: glEveryoneDiscovery}, true
		case req.GraphQL && strings.Contains(string(req.Body), "mergedAt"):
			return glCanned{Status: http.StatusOK, Body: glRecentMRsAnswer}, true
		case req.GraphQL && strings.Contains(string(req.Body), "mergeRequestInteraction"):
			return glCanned{Status: http.StatusOK, Body: glOpenMRReviewersAnswer}, true
		case req.GraphQL && strings.Contains(string(req.Body), "authoredMergeRequests"):
			return glCanned{Status: http.StatusOK, Body: glOpenMRsAnswer}, true
		case req.GraphQL:
			return glCanned{Status: http.StatusOK, Body: glEveryoneProjects}, true
		}
		return glCanned{}, false
	}

	snap := snapshotJSON(t, agentd.BuildDashboardHandlerForTest(), "/api/snapshot")
	_, present := snap["authored_open_mrs"]
	assert.False(t, present, "hidden until an instance has answered")

	_, err := agentd.PollAuthoredOpenMRsForTest()
	require.NoError(t, err)

	calls := gl.requests()
	require.Len(t, calls, 5, "mine, its reviewers, discovery, one query for the project, and the recently closed")
	for _, call := range calls {
		assert.Equal(t, glTestBase, call.Base, "the self-hosted instance, port included")
	}
	assert.True(t, calls[0].GraphQL)
	assert.Equal(t, "POST", calls[0].Method)
	assert.NotContains(t, string(calls[0].Body), "mergeRequestInteraction",
		"the list query never asks for a field an older instance lacks")
	assert.Contains(t, string(calls[1].Body), "mergeRequestInteraction", "a separate query does")
	assert.Equal(t, "merge_requests", calls[2].Path)
	assert.Equal(t, "all", calls[2].Query.Get("scope"))
	assert.Contains(t, string(calls[3].Body), `"p0":"contoso/sub/player"`)
	assert.Contains(t, string(calls[4].Body), "mergedAt")
	assert.Equal(t, []string{glTestToken, glTestToken, glTestToken, glTestToken, glTestToken}, gl.tokens)

	snap = snapshotJSON(t, agentd.BuildDashboardHandlerForTest(), "/api/snapshot")
	mrs, ok := snap["authored_open_mrs"].(map[string]any)
	require.True(t, ok, "snapshot=%v", snap["authored_open_mrs"])
	assert.Equal(t, true, mrs["available"])
	assert.EqualValues(t, 2, mrs["total"])

	items, ok := mrs["items"].([]any)
	require.True(t, ok)
	require.Len(t, items, 2)
	ready := items[0].(map[string]any)
	assert.EqualValues(t, 7, ready["number"])
	assert.Equal(t, "contoso/sub/player", ready["repository"])
	assert.Equal(t, "passing", ready["checks"].(map[string]any)["state"])
	readyReviews := ready["reviews"].(map[string]any)
	assert.EqualValues(t, 1, readyReviews["approved"])
	assert.EqualValues(t, 1, readyReviews["changes_requested"], "the reviewer query's change request")
	require.Len(t, readyReviews["reviewers"], 2)
	draft := items[1].(map[string]any)
	assert.Equal(t, true, draft["draft"])
	assert.Equal(t, true, draft["conflicts"])
	assert.Equal(t, "pending", draft["checks"].(map[string]any)["state"])
	assert.EqualValues(t, 3, mrs["recent_window_days"], "the pull requests' window, by default")
	recent, ok := mrs["recent"].([]any)
	require.True(t, ok)
	require.Len(t, recent, 1)
	assert.Equal(t, "merged", recent[0].(map[string]any)["state"])
	assert.EqualValues(t, 5, recent[0].(map[string]any)["number"])

	instances, ok := mrs["instances"].([]any)
	require.True(t, ok)
	require.Len(t, instances, 1)
	inst := instances[0].(map[string]any)
	assert.Equal(t, "gitlab.contoso.test:8929", inst["host"])
	assert.Equal(t, glTestBase+"/dashboard/merge_requests?state=opened&author_username=jdoe", inst["url"])
	assert.NotContains(t, inst, "error")

	assert.EqualValues(t, 3, mrs["everyone_total"], "the footer's second number: everyone's")
	projects, ok := mrs["projects"].([]any)
	require.True(t, ok)
	require.Len(t, projects, 1)
	project := projects[0].(map[string]any)
	assert.Equal(t, "contoso/sub/player", project["project"])
	assert.Equal(t, glTestBase+"/contoso/sub/player/-/merge_requests", project["url"])
	rows := project["items"].([]any)
	require.Len(t, rows, 3)
	assert.Equal(t, "martin", rows[2].(map[string]any)["author"])
	mine := rows[0].(map[string]any)
	require.EqualValues(t, 7, mine["number"])
	assert.EqualValues(t, 1, mine["reviews"].(map[string]any)["changes_requested"],
		"a project tab shows the reviews already read for the user's own merge request")
}

// glRecentMRsAnswer is the recently closed read: one merge request merged an
// hour before the test runs (the window is days, so a fixed date would age out).
var glRecentMRsAnswer = `{"data":{"currentUser":{
"merged":{"count":1,"nodes":[{"iid":"5","title":"Landed","webUrl":"https://gitlab.contoso.test:8929/contoso/sub/player/-/merge_requests/5",
 "updatedAt":"` + time.Now().Add(-time.Hour).UTC().Format(time.RFC3339) + `","mergedAt":"` + time.Now().Add(-time.Hour).UTC().Format(time.RFC3339) + `",
 "headPipeline":{"status":"SUCCESS"}}]},
"closed":{"count":0,"nodes":[]}}}}`

const glOpenMRReviewersAnswer = `{"data":{"currentUser":{"authoredMergeRequests":{"nodes":[
{"webUrl":"https://gitlab.contoso.test:8929/contoso/sub/player/-/merge_requests/7",
 "approvedBy":{"nodes":[{"username":"ann","bot":false}]},
 "reviewers":{"nodes":[{"username":"martin","bot":false,"mergeRequestInteraction":{"reviewState":"REQUESTED_CHANGES"}}]}},
{"webUrl":"https://gitlab.contoso.test:8929/contoso/sub/player/-/merge_requests/8",
 "approvedBy":{"nodes":[]},"reviewers":{"nodes":[]}}
]}}}}`

// TestOpenMRs_AnInstanceWithoutReviewerStatesShowsApprovals: the reviewer
// query fails on an older instance; the list still shows, with approvals, and
// the query is not sent again.
func TestOpenMRs_AnInstanceWithoutReviewerStatesShowsApprovals(t *testing.T) {
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_, _, gl := glWorld(t, []string{"gitlab.contoso.test/contoso"})
	t.Cleanup(agentd.ResetAuthoredOpenMRsForTest)
	reviewerReads := 0
	gl.route = func(req agentd.GitLabRequestForTest) (glCanned, bool) {
		switch {
		case !req.GraphQL && req.Path == "merge_requests":
			return glCanned{Status: http.StatusOK, Body: glEveryoneDiscovery}, true
		case req.GraphQL && strings.Contains(string(req.Body), "mergeRequestInteraction"):
			reviewerReads++
			return glCanned{Status: http.StatusOK, Body: `{"errors":[{"message":"Field 'mergeRequestInteraction' doesn't exist on type 'MergeRequestReviewer'",
				"extensions":{"code":"undefinedField"}}]}`}, true
		case req.GraphQL && strings.Contains(string(req.Body), "authoredMergeRequests"):
			return glCanned{Status: http.StatusOK, Body: glOpenMRsAnswer}, true
		case req.GraphQL:
			return glCanned{Status: http.StatusOK, Body: glEveryoneProjects}, true
		}
		return glCanned{}, false
	}

	_, err := agentd.PollAuthoredOpenMRsForTest()
	require.NoError(t, err, "a missing reviewer field is not a failed poll")
	_, err = agentd.PollAuthoredOpenMRsForTest()
	require.NoError(t, err)
	assert.Equal(t, 1, reviewerReads, "the instance's missing fields are remembered")

	snap := snapshotJSON(t, agentd.BuildDashboardHandlerForTest(), "/api/snapshot")
	mrs, ok := snap["authored_open_mrs"].(map[string]any)
	require.True(t, ok, "snapshot=%v", snap["authored_open_mrs"])
	items := mrs["items"].([]any)
	require.Len(t, items, 2)
	reviews := items[0].(map[string]any)["reviews"].(map[string]any)
	assert.EqualValues(t, 1, reviews["approved"])
	reviewers := reviews["reviewers"].([]any)
	require.Len(t, reviewers, 1)
	assert.Equal(t, "ann", reviewers[0].(map[string]any)["login"])
	assert.Equal(t, glTestBase+"/ann", reviewers[0].(map[string]any)["url"])
}

const glEveryoneDiscovery = `[
{"web_url":"https://gitlab.contoso.test:8929/contoso/sub/player/-/merge_requests/9"},
{"web_url":"https://gitlab.contoso.test:8929/contoso/sub/player/-/merge_requests/8"},
{"web_url":"https://gitlab.contoso.test:8929/contoso/sub/player/-/merge_requests/7"}]`

const glEveryoneProjects = `{"data":{"p0":{"mergeRequests":{"count":3,"nodes":[
{"iid":"7","title":"Add a thing","webUrl":"https://gitlab.contoso.test:8929/contoso/sub/player/-/merge_requests/7",
 "draft":false,"updatedAt":"2026-11-02T10:00:00Z","author":{"username":"jdoe"},"headPipeline":{"status":"SUCCESS"},"approvedBy":{"nodes":[]}},
{"iid":"8","title":"Draft: Try a thing","webUrl":"https://gitlab.contoso.test:8929/contoso/sub/player/-/merge_requests/8",
 "draft":true,"updatedAt":"2026-11-02T11:00:00Z","author":{"username":"jdoe"},"headPipeline":{"status":"RUNNING"},"approvedBy":{"nodes":[]}},
{"iid":"9","title":"Someone else's","webUrl":"https://gitlab.contoso.test:8929/contoso/sub/player/-/merge_requests/9",
 "draft":false,"updatedAt":"2026-11-02T12:00:00Z","author":{"username":"martin"},"headPipeline":null,"approvedBy":{"nodes":[]}}
]}}}}`

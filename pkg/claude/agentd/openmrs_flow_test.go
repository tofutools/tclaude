package agentd_test

import (
	"net/http"
	"strings"
	"testing"

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
	require.Len(t, calls, 3, "mine, discovery, and one query for the project")
	for _, call := range calls {
		assert.Equal(t, glTestBase, call.Base, "the self-hosted instance, port included")
	}
	assert.True(t, calls[0].GraphQL)
	assert.Equal(t, "POST", calls[0].Method)
	assert.Equal(t, "merge_requests", calls[1].Path)
	assert.Equal(t, "all", calls[1].Query.Get("scope"))
	assert.Contains(t, string(calls[2].Body), `"p0":"contoso/sub/player"`)
	assert.Equal(t, []string{glTestToken, glTestToken, glTestToken}, gl.tokens)

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
	assert.EqualValues(t, 1, ready["reviews"].(map[string]any)["approved"])
	draft := items[1].(map[string]any)
	assert.Equal(t, true, draft["draft"])
	assert.Equal(t, true, draft["conflicts"])
	assert.Equal(t, "pending", draft["checks"].(map[string]any)["state"])

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

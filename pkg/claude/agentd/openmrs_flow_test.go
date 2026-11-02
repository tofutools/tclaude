package agentd_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
)

// openmrs_flow_test.go: the footer's Open MRs list end to end. One poll
// against the stubbed GitLab transport, read back through the real dashboard
// snapshot handler.

const glOpenMRsAnswer = `{"data":{"currentUser":{"username":"yorick","authoredMergeRequests":{"count":2,"nodes":[
{"iid":"7","title":"Add a thing","webUrl":"https://gitlab.yamzz.test:6080/yamzz/sub/previewplayback/-/merge_requests/7",
 "draft":false,"updatedAt":"2026-11-02T10:00:00Z","conflicts":false,"detailedMergeStatus":"MERGEABLE",
 "headPipeline":{"status":"SUCCESS"},"approvedBy":{"nodes":[{"username":"ann"}]}},
{"iid":"8","title":"Draft: Try a thing","webUrl":"https://gitlab.yamzz.test:6080/yamzz/sub/previewplayback/-/merge_requests/8",
 "draft":true,"updatedAt":"2026-11-02T11:00:00Z","conflicts":true,"detailedMergeStatus":"CONFLICT",
 "headPipeline":{"status":"RUNNING"},"approvedBy":{"nodes":[]}}
]}}}}`

func TestOpenMRs_SnapshotCarriesTheTokenUsersMergeRequests(t *testing.T) {
	// The dashboard handler checks Origin against the popup base. Set it here
	// rather than relying on an earlier test leaving the global set, which
	// holds in a full package run but not under -run.
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	_, _, gl := glWorld(t, []string{"gitlab.yamzz.test/yamzz"})
	t.Cleanup(agentd.ResetAuthoredOpenMRsForTest)
	gl.route = func(req agentd.GitLabRequestForTest) (glCanned, bool) {
		if req.GraphQL {
			return glCanned{Status: http.StatusOK, Body: glOpenMRsAnswer}, true
		}
		return glCanned{}, false
	}

	snap := snapshotJSON(t, agentd.BuildDashboardHandlerForTest(), "/api/snapshot")
	_, present := snap["authored_open_mrs"]
	assert.False(t, present, "hidden until an instance has answered")

	_, err := agentd.PollAuthoredOpenMRsForTest()
	require.NoError(t, err)

	call := gl.only(t)
	assert.True(t, call.GraphQL)
	assert.Equal(t, glTestBase, call.Base, "the self-hosted instance, port included")
	assert.Equal(t, "POST", call.Method)
	assert.Equal(t, []string{glTestToken}, gl.tokens)

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
	assert.Equal(t, "yamzz/sub/previewplayback", ready["repository"])
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
	assert.Equal(t, "gitlab.yamzz.test:6080", inst["host"])
	assert.Equal(t, glTestBase+"/dashboard/merge_requests?state=opened&author_username=yorick", inst["url"])
	assert.NotContains(t, inst, "error")
}

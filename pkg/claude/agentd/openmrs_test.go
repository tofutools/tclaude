package agentd

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
)

// openmrs_test.go covers the Open MRs poller's pure pieces and its lifecycle
// against a stubbed GitLab transport. openmrs_flow_test.go reads the result
// through the real dashboard snapshot.

const testMRBase = "https://gitlab.example.test:6080"

func testMRNode(iid int, mutate func(*glOpenMRNode)) glOpenMRNode {
	n := glOpenMRNode{
		IID:       strconv.Itoa(iid),
		Title:     "Change " + strconv.Itoa(iid),
		WebURL:    testMRBase + "/acme/sub/app/-/merge_requests/" + strconv.Itoa(iid),
		UpdatedAt: "2026-11-02T10:00:00Z",
	}
	if mutate != nil {
		mutate(&n)
	}
	return n
}

func pipelineStatus(s string) *struct {
	Status string `json:"status"`
} {
	return &struct {
		Status string `json:"status"`
	}{Status: s}
}

func TestAuthoredOpenMRItem_MapsAMergeRequestOntoAFooterRow(t *testing.T) {
	node := testMRNode(7, func(n *glOpenMRNode) {
		n.WebURL += "/diffs" // a sub-path still names the merge request
		n.Draft = true
		n.Conflicts = true
		n.DetailedMergeStatus = "REQUESTED_CHANGES"
		n.HeadPipeline = pipelineStatus("SUCCESS")
		n.ApprovedBy.Nodes = append(n.ApprovedBy.Nodes,
			struct {
				Username string `json:"username"`
			}{"ann"},
			struct {
				Username string `json:"username"`
			}{"bob"})
	})
	item, ok := authoredOpenMRItem(testMRBase, node, "2026-11-02T10:05:00Z")
	require.True(t, ok)
	assert.Equal(t, 7, item.Number)
	assert.Equal(t, testMRBase+"/acme/sub/app/-/merge_requests/7", item.URL, "the canonical URL, sub-path dropped")
	assert.Equal(t, "acme/sub/app", item.Repository, "the full nested project path")
	assert.True(t, item.Draft)
	assert.True(t, item.Conflicts)
	require.NotNil(t, item.Checks)
	assert.Equal(t, "passing", item.Checks.State)
	require.NotNil(t, item.Reviews)
	assert.Equal(t, 2, item.Reviews.Approved, "real approvals, counted")
	assert.Equal(t, 1, item.Reviews.ChangesRequested)
	assert.True(t, item.Reviews.ReviewersKnown)
	assert.Equal(t, "2026-11-02T10:00:00Z", item.UpdatedAt)

	t.Run("no approvals is zero, whatever the project requires", func(t *testing.T) {
		item, ok := authoredOpenMRItem(testMRBase, testMRNode(8, nil), "")
		require.True(t, ok)
		assert.Equal(t, 0, item.Reviews.Approved)
		assert.Nil(t, item.Checks, "no pipeline is no checks")
	})

	refused := map[string]glOpenMRNode{
		"another host": testMRNode(9, func(n *glOpenMRNode) {
			n.WebURL = "https://evil.example.test:6080/acme/sub/app/-/merge_requests/9"
		}),
		"another port": testMRNode(9, func(n *glOpenMRNode) {
			n.WebURL = "https://gitlab.example.test:9999/acme/sub/app/-/merge_requests/9"
		}),
		"not a merge request URL": testMRNode(9, func(n *glOpenMRNode) {
			n.WebURL = testMRBase + "/acme/sub/app/-/issues/9"
		}),
		"an iid the URL does not carry": testMRNode(9, func(n *glOpenMRNode) { n.IID = "10" }),
	}
	for name, node := range refused {
		t.Run("refused: "+name, func(t *testing.T) {
			_, ok := authoredOpenMRItem(testMRBase, node, "")
			assert.False(t, ok)
		})
	}
}

func TestGLPipelineChecks(t *testing.T) {
	assert.Nil(t, glPipelineChecks(nil, ""))
	assert.Nil(t, glPipelineChecks(pipelineStatus("SKIPPED"), ""), "a skipped pipeline ran nothing")
	for status, want := range map[string]string{
		"SUCCESS": "passing", "FAILED": "failing", "CANCELED": "failing",
		"RUNNING": "pending", "PENDING": "pending", "CREATED": "pending", "MANUAL": "pending",
		"WAITING_FOR_RESOURCE": "pending", "SOMETHING_NEW": "pending",
	} {
		got := glPipelineChecks(pipelineStatus(status), "")
		require.NotNil(t, got, status)
		assert.Equal(t, want, got.State, status)
		assert.Equal(t, 1, got.Total, "%s is one pipeline-level check", status)
	}
}

func TestGLRetryAfter(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	h := func(k, v string) glAPIResult { return glAPIResult{Header: http.Header{k: []string{v}}} }
	assert.Equal(t, 42*time.Second, glRetryAfter(h("Retry-After", "42"), now))
	assert.Equal(t, 90*time.Second, glRetryAfter(h("Ratelimit-Reset", strconv.FormatInt(now.Unix()+90, 10)), now))
	assert.Equal(t, authoredOpenMRDefaultRetry, glRetryAfter(glAPIResult{Header: http.Header{}}, now))
}

func TestGLRequestURL_GraphQL(t *testing.T) {
	got, err := glRequestURL(testMRBase+"/", glAPIRequest{GraphQL: true})
	require.NoError(t, err)
	assert.Equal(t, testMRBase+"/api/graphql", got)
	_, err = glRequestURL(testMRBase, glAPIRequest{GraphQL: true, Path: "projects/1"})
	assert.Error(t, err, "a GraphQL request names no REST path")
}

// openMRsWorld configures instances and a token, and stubs the transport with
// one answer per base.
func openMRsWorld(t *testing.T, hosts map[string]string, answer func(base string) glAPIResult) *[]string {
	t.Helper()
	setupTestDB(t)
	tokenPath := filepath.Join(t.TempDir(), "gitlab-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("glpat-open-mrs\n"), 0o600))
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{GitProxy: &config.GitProxyConfig{
		AllowedRemotes:  []string{"gitlab.example.test"},
		GitLabHosts:     hosts,
		GitLabTokenFile: tokenPath,
	}}}))
	t.Cleanup(func() { resetAuthoredOpenMRs("") })
	var mu sync.Mutex
	var bases []string
	t.Cleanup(SetGitLabTransportForTest(func(_ context.Context, base, token string, req glAPIRequest) (glAPIResult, error) {
		mu.Lock()
		bases = append(bases, base)
		mu.Unlock()
		require.Equal(t, "glpat-open-mrs", token)
		require.True(t, req.GraphQL)
		body, _ := json.Marshal(req.Body)
		require.Contains(t, string(body), "authoredMergeRequests")
		res := answer(base)
		if res.Header == nil {
			res.Header = http.Header{}
		}
		return res, nil
	}))
	return &bases
}

func graphQLAnswer(t *testing.T, username string, count int, nodes ...glOpenMRNode) glAPIResult {
	t.Helper()
	doc := map[string]any{"data": map[string]any{"currentUser": map[string]any{
		"username":              username,
		"authoredMergeRequests": map[string]any{"count": count, "nodes": nodes},
	}}}
	body, err := json.Marshal(doc)
	require.NoError(t, err)
	return glAPIResult{Status: http.StatusOK, Body: body}
}

func loadCfg(t *testing.T) *config.Config {
	t.Helper()
	cfg, err := config.Load()
	require.NoError(t, err)
	return cfg
}

func TestPollAuthoredOpenMRs_MergesInstancesAndKeepsTheLastGoodList(t *testing.T) {
	const other = "https://other.example.test"
	fail := false
	openMRsWorld(t, map[string]string{"gitlab.example.test": testMRBase, "other.example.test": other},
		func(base string) glAPIResult {
			if base == other {
				return glAPIResult{Status: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{"120"}}}
			}
			if fail {
				return glAPIResult{Status: http.StatusBadGateway, Body: []byte(`{"message":"bad gateway"}`)}
			}
			return graphQLAnswer(t, "yorick", 3, testMRNode(7, nil), testMRNode(8, nil))
		})

	wait, err := pollAuthoredOpenMRs(context.Background())
	require.NoError(t, err, "one instance answering is a good poll")
	assert.Equal(t, 120*time.Second, wait, "the rate-limited instance's wait is honoured")

	mrURL := testMRBase + "/acme/sub/app/-/merge_requests/7"
	agents := []dashboardAgent{{AgentID: "agt_mr", Title: "worker", repoLinksView: repoLinksView{BranchPRURL: mrURL}}}
	view := authoredOpenMRsSnapshot(loadCfg(t), agents)
	require.NotNil(t, view)
	assert.True(t, view.Available)
	assert.Equal(t, 3, view.Total, "GitLab's count, beyond the page")
	assert.True(t, view.Truncated)
	require.Len(t, view.Items, 2)
	assert.Equal(t, "agt_mr", view.Items[0].AgentID, "an agent's branch MR attaches the row")
	assert.Empty(t, view.Items[1].AgentID, "the other stays unattached")
	require.Len(t, view.Instances, 2)
	byBase := map[string]dashboardAuthoredOpenMRInstance{}
	for _, inst := range view.Instances {
		byBase[inst.Base] = inst
	}
	assert.Equal(t, "yorick", byBase[testMRBase].Username)
	assert.Equal(t, testMRBase+"/dashboard/merge_requests?state=opened&author_username=yorick", byBase[testMRBase].URL)
	assert.Equal(t, "gitlab.example.test:6080", byBase[testMRBase].Host)
	assert.Contains(t, byBase[other].Error, "rate limit")
	assert.Empty(t, byBase[other].URL)

	fail = true
	_, err = pollAuthoredOpenMRs(context.Background())
	require.Error(t, err, "no instance answered")
	view = authoredOpenMRsSnapshot(loadCfg(t), nil)
	require.NotNil(t, view, "a failed poll keeps the last good list")
	assert.Len(t, view.Items, 2)
}

func TestAuthoredOpenMRsSnapshot_ShowsOnlyForTheCurrentConfiguration(t *testing.T) {
	openMRsWorld(t, map[string]string{"gitlab.example.test": testMRBase}, func(string) glAPIResult {
		return graphQLAnswer(t, "yorick", 1, testMRNode(7, nil))
	})
	require.Nil(t, authoredOpenMRsSnapshot(loadCfg(t), nil), "nothing until an instance answers")
	_, err := pollAuthoredOpenMRs(context.Background())
	require.NoError(t, err)
	require.NotNil(t, authoredOpenMRsSnapshot(loadCfg(t), nil))

	cfg := loadCfg(t)
	cfg.Agent.GitProxy.GitLabHosts = map[string]string{"gitlab.example.test": "https://gitlab.example.test:7070"}
	assert.Nil(t, authoredOpenMRsSnapshot(cfg, nil), "a list read under other instances is not shown")
	cfg = loadCfg(t)
	cfg.Agent.GitProxy.GitLabTokenFile = "/another/token"
	assert.Nil(t, authoredOpenMRsSnapshot(cfg, nil), "nor one read with another token")
	cfg = loadCfg(t)
	cfg.Agent.GitProxy.GitLabHosts = nil
	assert.Nil(t, authoredOpenMRsSnapshot(cfg, nil), "no gitlab_hosts entry, no indicator")
}

func TestPollAuthoredOpenMRs_SurfacesAGraphQLError(t *testing.T) {
	openMRsWorld(t, map[string]string{"gitlab.example.test": testMRBase}, func(string) glAPIResult {
		return glAPIResult{Status: http.StatusOK,
			Body: []byte(`{"errors":[{"message":"Field 'detailedMergeStatus' doesn't exist on type 'MergeRequest'"}]}`)}
	})
	_, err := pollAuthoredOpenMRs(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "detailedMergeStatus", "an old GitLab says which field it lacks")
	assert.Nil(t, authoredOpenMRsSnapshot(loadCfg(t), nil))
}

func TestPollAuthoredOpenMRs_NoTokenFileIsNotAFailure(t *testing.T) {
	setupTestDB(t)
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{GitProxy: &config.GitProxyConfig{
		AllowedRemotes: []string{"gitlab.example.test"},
		GitLabHosts:    map[string]string{"gitlab.example.test": testMRBase},
	}}}))
	t.Cleanup(func() { resetAuthoredOpenMRs("") })
	called := false
	t.Cleanup(SetGitLabTransportForTest(func(context.Context, string, string, glAPIRequest) (glAPIResult, error) {
		called = true
		return glAPIResult{}, nil
	}))
	wait, err := pollAuthoredOpenMRs(context.Background())
	assert.NoError(t, err)
	assert.Zero(t, wait)
	assert.False(t, called, "no token, no request")
	assert.Nil(t, authoredOpenMRsSnapshot(loadCfg(t), nil))
}

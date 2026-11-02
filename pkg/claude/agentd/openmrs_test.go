package agentd

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

const testMRBase = "https://gitlab.example.test:8929"

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
			n.WebURL = "https://evil.example.test:8929/acme/sub/app/-/merge_requests/9"
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
		body, _ := json.Marshal(req.Body)
		var res glAPIResult
		switch {
		case !req.GraphQL:
			// Project-wide discovery: nobody else has open merge requests.
			res = glAPIResult{Status: http.StatusOK, Body: []byte("[]")}
		case strings.Contains(string(body), "authoredMergeRequests"):
			res = answer(base)
		default:
			res = glAPIResult{Status: http.StatusOK, Body: []byte(`{"data":{}}`)}
		}
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
			return graphQLAnswer(t, "jdoe", 3, testMRNode(7, nil), testMRNode(8, nil))
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
	assert.Equal(t, "jdoe", byBase[testMRBase].Username)
	assert.Equal(t, testMRBase+"/dashboard/merge_requests?state=opened&author_username=jdoe", byBase[testMRBase].URL)
	assert.Equal(t, "gitlab.example.test:8929", byBase[testMRBase].Host)
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
		return graphQLAnswer(t, "jdoe", 1, testMRNode(7, nil))
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

// TestEveryoneOpenMRs_GroupsTheInstancesOpenMergeRequestsByProject drives the
// project-wide half: REST discovery names the projects, one GraphQL query per
// chunk reads them, and the snapshot orders the tabs with the projects agents
// work in first.
func TestEveryoneOpenMRs_GroupsTheInstancesOpenMergeRequestsByProject(t *testing.T) {
	setupTestDB(t)
	tokenPath := filepath.Join(t.TempDir(), "gitlab-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("glpat-open-mrs\n"), 0o600))
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{GitProxy: &config.GitProxyConfig{
		AllowedRemotes:  []string{"gitlab.example.test"},
		GitLabHosts:     map[string]string{"gitlab.example.test": testMRBase},
		GitLabTokenFile: tokenPath,
	}}}))
	t.Cleanup(func() { resetAuthoredOpenMRs("") })

	mr := func(project string, iid int, author string, mutate func(map[string]any)) map[string]any {
		n := map[string]any{
			"iid": strconv.Itoa(iid), "title": "Change " + strconv.Itoa(iid),
			"webUrl": testMRBase + "/" + project + "/-/merge_requests/" + strconv.Itoa(iid),
			"draft":  false, "updatedAt": "2026-11-02T10:00:00Z", "author": map[string]any{"username": author},
			"headPipeline": map[string]any{"status": "SUCCESS"}, "approvedBy": map[string]any{"nodes": []any{}},
		}
		if mutate != nil {
			mutate(n)
		}
		return n
	}
	var discovery []url.Values
	var projectQueries []map[string]any
	t.Cleanup(SetGitLabTransportForTest(func(_ context.Context, base, token string, req glAPIRequest) (glAPIResult, error) {
		require.Equal(t, testMRBase, base)
		if !req.GraphQL {
			require.Equal(t, "merge_requests", req.Path)
			discovery = append(discovery, req.Query)
			list := []map[string]any{
				{"web_url": testMRBase + "/contoso/player/-/merge_requests/249"},
				{"web_url": testMRBase + "/contoso/player/-/merge_requests/250"},
				{"web_url": testMRBase + "/tools/ci/-/merge_requests/3"},
				{"web_url": "https://elsewhere.example.test/x/y/-/merge_requests/1"},
			}
			body, _ := json.Marshal(list)
			return glAPIResult{Status: http.StatusOK, Body: body}, nil
		}
		body, _ := json.Marshal(req.Body)
		if strings.Contains(string(body), "authoredMergeRequests") {
			return graphQLAnswer(t, "jdoe", 1, testMRNode(7, func(n *glOpenMRNode) {
				n.WebURL = testMRBase + "/tools/ci/-/merge_requests/7"
			})), nil
		}
		var payload struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		require.NoError(t, json.Unmarshal(body, &payload))
		projectQueries = append(projectQueries, payload.Variables)
		assert.NotContains(t, payload.Query, "player", "project paths travel as variables, never in the query text")
		data := map[string]any{}
		for alias, path := range payload.Variables {
			switch path {
			case "contoso/player":
				data[alias] = map[string]any{"mergeRequests": map[string]any{"count": 2, "nodes": []any{
					mr("contoso/player", 249, "martin", nil),
					mr("contoso/player", 250, "agent-bot", func(n map[string]any) { n["conflicts"] = true }),
				}}}
			case "tools/ci":
				data[alias] = map[string]any{"mergeRequests": map[string]any{"count": 3, "nodes": []any{
					mr("tools/ci", 3, "ann", nil),
					mr("tools/ci", 7, "jdoe", nil),
					mr("tools/ci", 8, "bob", nil),
				}}}
			}
		}
		out, _ := json.Marshal(map[string]any{"data": data})
		return glAPIResult{Status: http.StatusOK, Body: out}, nil
	}))

	_, err := pollAuthoredOpenMRs(context.Background())
	require.NoError(t, err)
	require.Len(t, discovery, 1, "a short page ends discovery")
	assert.Equal(t, "all", discovery[0].Get("scope"))
	assert.Equal(t, "opened", discovery[0].Get("state"))
	require.Len(t, projectQueries, 1, "two projects fit one GraphQL query")

	agents := []dashboardAgent{{AgentID: "agt_pp", Title: "builder", repoLinksView: repoLinksView{
		BranchURL: testMRBase + "/contoso/player/-/tree/headless"}}}
	view := authoredOpenMRsSnapshot(loadCfg(t), agents)
	require.NotNil(t, view)
	assert.Equal(t, 1, view.Total, "the footer's first number stays yours")
	assert.Equal(t, 5, view.EveryoneTotal, "the second counts everyone's")
	require.Len(t, view.Projects, 2)
	assert.Equal(t, "contoso/player", view.Projects[0].Project, "the project an agent works in comes first")
	assert.True(t, view.Projects[0].Agents)
	assert.Equal(t, "tools/ci", view.Projects[1].Project, "busier, but no agent works in it")
	assert.Equal(t, testMRBase+"/contoso/player/-/merge_requests", view.Projects[0].URL)
	pp := view.Projects[0].Items
	require.Len(t, pp, 2)
	assert.Equal(t, "martin", pp[0].Author)
	assert.Equal(t, "agent-bot", pp[1].Author)
	assert.True(t, pp[1].Conflicts)
	assert.Equal(t, "passing", pp[0].Checks.State)
}

func TestFetchEveryoneOpenMRs_StopsDiscoveryAtItsCap(t *testing.T) {
	pages := 0
	t.Cleanup(SetGitLabTransportForTest(func(_ context.Context, _, _ string, req glAPIRequest) (glAPIResult, error) {
		if req.GraphQL {
			return glAPIResult{Status: http.StatusOK, Body: []byte(`{"data":{}}`)}, nil
		}
		pages++
		list := make([]map[string]any, 100)
		for i := range list {
			list[i] = map[string]any{"web_url": testMRBase + "/acme/app/-/merge_requests/" + strconv.Itoa(pages*1000+i+1)}
		}
		body, _ := json.Marshal(list)
		return glAPIResult{Status: http.StatusOK, Body: body}, nil
	}))
	got, _, err := fetchEveryoneOpenMRs(context.Background(), testMRBase, "glpat", nil)
	require.NoError(t, err)
	assert.Equal(t, everyoneOpenMRPages, pages)
	assert.True(t, got.truncated, "full pages up to the cap mean there may be more")
}

func TestFetchEveryoneOpenMRs_CapsTheProjectsReadAgentsFirst(t *testing.T) {
	// 30 projects with open merge requests; p29 has one, but an agent works
	// in it.
	var graphQL []map[string]any
	t.Cleanup(SetGitLabTransportForTest(func(_ context.Context, _, _ string, req glAPIRequest) (glAPIResult, error) {
		if req.GraphQL {
			body, _ := json.Marshal(req.Body)
			var payload struct {
				Variables map[string]any `json:"variables"`
			}
			require.NoError(t, json.Unmarshal(body, &payload))
			graphQL = append(graphQL, payload.Variables)
			return glAPIResult{Status: http.StatusOK, Body: []byte(`{"data":{}}`)}, nil
		}
		// One short page: every project once, then p0 nine more times and
		// p1 four more, so p0 is the busiest and p1 the next.
		var page []map[string]any
		add := func(p, n int) {
			page = append(page, map[string]any{"web_url": testMRBase + "/acme/p" + strconv.Itoa(p) +
				"/-/merge_requests/" + strconv.Itoa(n)})
		}
		for p := range 30 {
			add(p, 1)
		}
		for n := 2; n <= 10; n++ {
			add(0, n)
		}
		for n := 2; n <= 5; n++ {
			add(1, n)
		}
		body, _ := json.Marshal(page)
		return glAPIResult{Status: http.StatusOK, Body: body}, nil
	}))
	agents := map[string]bool{strings.ToLower(testMRBase + "/acme/p29"): true}
	got, _, err := fetchEveryoneOpenMRs(context.Background(), testMRBase, "glpat", agents)
	require.NoError(t, err)
	assert.True(t, got.truncated, "projects beyond the cap make the count a floor")
	require.Len(t, graphQL, everyoneOpenMRMaxProjects/everyoneOpenMRChunk)
	var read []string
	for _, vars := range graphQL {
		for i := range len(vars) {
			read = append(read, vars["p"+strconv.Itoa(i)].(string))
		}
	}
	require.Len(t, read, everyoneOpenMRMaxProjects)
	assert.Equal(t, "acme/p29", read[0], "the project an agent works in is read first, however quiet")
	assert.Equal(t, []string{"acme/p0", "acme/p1", "acme/p2"}, read[1:4], "then the busiest, then newest first")
}

func TestPollAuthoredOpenMRs_AFailedEveryoneStepKeepsTheTabsAndTheInstance(t *testing.T) {
	limited := false
	openMRsWorld(t, map[string]string{"gitlab.example.test": testMRBase}, func(string) glAPIResult {
		return graphQLAnswer(t, "jdoe", 0)
	})
	// Replace the world's default everyone answers: one project, then a 429.
	t.Cleanup(SetGitLabTransportForTest(func(_ context.Context, _, _ string, req glAPIRequest) (glAPIResult, error) {
		body, _ := json.Marshal(req.Body)
		switch {
		case req.GraphQL && strings.Contains(string(body), "authoredMergeRequests"):
			return graphQLAnswer(t, "jdoe", 0), nil
		case limited:
			return glAPIResult{Status: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"90"}}}, nil
		case !req.GraphQL:
			return glAPIResult{Status: http.StatusOK,
				Body: []byte(`[{"web_url":"` + testMRBase + `/acme/app/-/merge_requests/1"}]`)}, nil
		}
		return glAPIResult{Status: http.StatusOK, Body: []byte(`{"data":{"p0":{"mergeRequests":{"count":1,"nodes":[` +
			`{"iid":"1","title":"One","webUrl":"` + testMRBase + `/acme/app/-/merge_requests/1","author":{"username":"ann"}}]}}}}`)}, nil
	}))
	_, err := pollAuthoredOpenMRs(context.Background())
	require.NoError(t, err)
	view := authoredOpenMRsSnapshot(loadCfg(t), nil)
	require.NotNil(t, view)
	require.Len(t, view.Projects, 1)

	limited = true
	wait, err := pollAuthoredOpenMRs(context.Background())
	require.NoError(t, err, "your own list answered, so the poll is good")
	assert.Equal(t, 90*time.Second, wait, "the everyone step's rate limit is honoured")
	view = authoredOpenMRsSnapshot(loadCfg(t), nil)
	require.NotNil(t, view)
	assert.Len(t, view.Projects, 1, "the last good project tabs stay")
	require.Len(t, view.Instances, 1)
	assert.Empty(t, view.Instances[0].Error, "the instance itself is fine")
	assert.Contains(t, view.Instances[0].EveryoneError, "rate limit")
}

func TestAuthoredOpenMRNextDelay(t *testing.T) {
	delay, failures := authoredOpenMRNextDelay(90*time.Second, errors.New("limited"), 2)
	assert.Equal(t, 90*time.Second, delay, "a rate limit waits what GitLab asked")
	assert.Equal(t, 2, failures, "and is not a failure")
	delay, _ = authoredOpenMRNextDelay(time.Second, nil, 0)
	assert.Equal(t, authoredOpenMRPollInterval, delay, "never sooner than the poll interval")
	delay, _ = authoredOpenMRNextDelay(time.Hour, nil, 0)
	assert.Equal(t, authoredOpenMRMaxBackoff, delay, "never longer than the backoff cap")
	delay, failures = authoredOpenMRNextDelay(0, errors.New("down"), 0)
	assert.Equal(t, 1, failures)
	assert.Equal(t, 2*authoredOpenMRPollInterval, delay, "errors back off")
	delay, failures = authoredOpenMRNextDelay(0, nil, 3)
	assert.Equal(t, authoredOpenMRPollInterval, delay)
	assert.Equal(t, 0, failures, "success resets")
}

func TestAuthoredOpenMRItem_RequiredApprovalsStillMissing(t *testing.T) {
	item, ok := authoredOpenMRItem(testMRBase, testMRNode(7, func(n *glOpenMRNode) {
		n.DetailedMergeStatus = "NOT_APPROVED"
		n.HeadPipeline = pipelineStatus("SUCCESS")
		n.ApprovedBy.Nodes = append(n.ApprovedBy.Nodes, struct {
			Username string `json:"username"`
		}{"ann"})
	}), "")
	require.True(t, ok)
	assert.Equal(t, 1, item.Reviews.Approved, "the real approval is still counted")
	assert.Equal(t, "review_required", item.Reviews.Decision, "but the project wants more, so it is not approved")
}

// TestPollAuthoredOpenMRs_ARewrittenTokenDropsTheOldAccountsLists: the same
// token FILE rewritten to another account must not keep showing the first
// account's merge requests through a failed poll.
func TestPollAuthoredOpenMRs_ARewrittenTokenDropsTheOldAccountsLists(t *testing.T) {
	fail := false
	openMRsWorld(t, map[string]string{"gitlab.example.test": testMRBase}, func(string) glAPIResult {
		if fail {
			return glAPIResult{Status: http.StatusBadGateway, Body: []byte(`{"message":"bad gateway"}`)}
		}
		return graphQLAnswer(t, "first-account", 1, testMRNode(7, nil))
	})
	_, err := pollAuthoredOpenMRs(context.Background())
	require.NoError(t, err)
	require.NotNil(t, authoredOpenMRsSnapshot(loadCfg(t), nil))

	cfg := loadCfg(t)
	require.NoError(t, os.WriteFile(cfg.Agent.GitProxy.GitLabTokenFile, []byte("glpat-open-mrs-second\n"), 0o600))
	fail = true
	// The world's transport insists on the first token; a fresh one that
	// accepts any token stands in for the second account's failing instance.
	t.Cleanup(SetGitLabTransportForTest(func(context.Context, string, string, glAPIRequest) (glAPIResult, error) {
		return glAPIResult{Status: http.StatusBadGateway, Body: []byte(`{"message":"bad gateway"}`), Header: http.Header{}}, nil
	}))
	_, err = pollAuthoredOpenMRs(context.Background())
	require.Error(t, err)
	assert.Nil(t, authoredOpenMRsSnapshot(loadCfg(t), nil), "nothing read with the new token: nothing shown")
}

func TestOpenMRProjectsSnapshot_APresentedMergeRequestMarksTheAgentsProject(t *testing.T) {
	everyone := map[string]authoredOpenMREveryone{testMRBase: {projects: []dashboardOpenMRProject{
		{Base: testMRBase, Project: "acme/busy", Total: 9, Items: []dashboardAuthoredOpenPR{}},
		{Base: testMRBase, Project: "acme/app", Total: 1, Items: []dashboardAuthoredOpenPR{}},
	}}}
	agents := []dashboardAgent{{AgentID: "agt_p", repoLinksView: repoLinksView{
		PresentedPRs: []presentedPRView{{URL: testMRBase + "/acme/app/-/merge_requests/3"}}}}}
	projects, _, _ := openMRProjectsSnapshot(everyone, agents)
	require.Len(t, projects, 2)
	assert.Equal(t, "acme/app", projects[0].Project, "an agent known only by a presented merge request still counts")
	assert.True(t, projects[0].Agents)
}

// TestRunServeStartsTheOpenMRsPoller pins the daemon wiring: nothing else
// starts the poller, so a lost call would leave the footer silently empty.
// It reads serve.go, as the proxy audit tests do, rather than booting a
// daemon.
func TestRunServeStartsTheOpenMRsPoller(t *testing.T) {
	source, err := os.ReadFile("serve.go")
	require.NoError(t, err)
	text := string(source)
	start := strings.Index(text, "\nfunc runServe(")
	require.GreaterOrEqual(t, start, 0, "runServe has moved")
	body := text[start+1:]
	if end := strings.Index(body, "\nfunc "); end >= 0 {
		body = body[:end]
	}
	assert.Contains(t, body, "startAuthoredOpenPRPoller(cronStop)")
	assert.Contains(t, body, "startAuthoredOpenMRPoller(cronStop)",
		"the Open MRs poller starts with the daemon, beside the Open PRs one")
}

// TestAuthoredOpenMRsSnapshot_HidesListsReadWithAnotherToken: between polls,
// the snapshot itself must not show the previous account's lists.
func TestAuthoredOpenMRsSnapshot_HidesListsReadWithAnotherToken(t *testing.T) {
	openMRsWorld(t, map[string]string{"gitlab.example.test": testMRBase}, func(string) glAPIResult {
		return graphQLAnswer(t, "first-account", 1, testMRNode(7, nil))
	})
	_, err := pollAuthoredOpenMRs(context.Background())
	require.NoError(t, err)
	require.NotNil(t, authoredOpenMRsSnapshot(loadCfg(t), nil))

	tokenFile := loadCfg(t).Agent.GitProxy.GitLabTokenFile
	require.NoError(t, os.WriteFile(tokenFile, []byte("glpat-open-mrs-second\n"), 0o600))
	assert.Nil(t, authoredOpenMRsSnapshot(loadCfg(t), nil), "no poll yet: the first account's list is not shown")

	require.NoError(t, os.WriteFile(tokenFile, []byte("glpat-open-mrs\n"), 0o600))
	assert.NotNil(t, authoredOpenMRsSnapshot(loadCfg(t), nil), "the token it was read with shows it again")

	require.NoError(t, os.Remove(tokenFile))
	assert.Nil(t, authoredOpenMRsSnapshot(loadCfg(t), nil), "an unreadable token shows nothing")
}

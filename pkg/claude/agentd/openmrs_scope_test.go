package agentd

import (
	"context"
	"encoding/json"
	"net/http"
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

// openmrs_scope_test.go: everyone's merge requests under a token limited to
// some projects, which may not call the instance-wide merge request list.

const glRefusedBody = `{"error":"insufficient_granular_scope"}`

func scopedPolicy(t *testing.T, allowed ...string) config.GitProxyConfig {
	t.Helper()
	cfg := &config.Config{Agent: &config.AgentConfig{GitProxy: &config.GitProxyConfig{
		AllowedRemotes: allowed,
		GitLabHosts: map[string]string{
			"gitlab.example.test": testMRBase,
			"other.example.test":  "https://other.example.test",
		},
	}}}
	return cfg.ResolvedGitProxy()
}

func TestEveryoneFallbackProjects_AgentsThenMineThenExactAllowedRemotes(t *testing.T) {
	policy := scopedPolicy(t,
		"gitlab.example.test/acme/allowed",
		"gitlab.example.test/Acme/Mine",       // the same project as a Mine one, other case
		"gitlab.example.test/acme/*",          // a wildcard names no project
		"gitlab.example.test/acme/*/deep",     // nor does one in the middle
		"gitlab.example.test/acme",            // a group, too short to be a project
		"gitlab.example.test",                 // the whole host
		"other.example.test/acme/elsewhere",   // another instance
		"github.com/acme/app",                 // not GitLab at all
		"gitlab.example.test/acme/sub/nested", // subgroups are fine
		"gitlab.example.test/acme/allowed",    // listed twice
	)
	mine := []dashboardAuthoredOpenPR{{Repository: "acme/mine"}, {Repository: "acme/agent"}}
	known := everyoneFallbackKnown(policy, testMRBase, mine)
	agents := map[string]bool{
		strings.ToLower(testMRBase + "/acme/agent"): true,
		strings.ToLower(testMRBase + "/acme/zeta"):  true,
		"https://other.example.test/acme/theirs":    true,
		"https://github.com/acme/app":               true,
		strings.ToLower(testMRBase + "/"):           true, // no project
	}
	got := everyoneFallbackProjects(testMRBase, agents, known)
	assert.Equal(t, []string{
		"acme/agent", "acme/zeta", // agents first, sorted
		"acme/mine", // then the user's own merge requests
		"acme/allowed", "acme/sub/nested",
	}, got, "deduplicated case-insensitively, on this instance only, wildcards ignored")
}

// scopedWorld stubs a GitLab whose instance-wide list answers 403 and whose
// GraphQL reads projects. It records the REST calls and the project paths
// each GraphQL project query asked for.
type scopedGitLab struct {
	mu          sync.Mutex
	listCalls   int
	listStatus  int
	projectRead [][]string
	// projectStatus, when set, is every project query's answer.
	projectStatus int
}

func (g *scopedGitLab) do(_ context.Context, _, _ string, req glAPIRequest) (glAPIResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !req.GraphQL {
		g.listCalls++
		if g.listStatus == http.StatusOK {
			return glAPIResult{Status: http.StatusOK, Body: []byte(`[{"web_url":"` + testMRBase + `/acme/listed/-/merge_requests/1"}]`), Header: http.Header{}}, nil
		}
		return glAPIResult{Status: g.listStatus, Body: []byte(glRefusedBody), Header: http.Header{}}, nil
	}
	body, _ := json.Marshal(req.Body)
	if strings.Contains(string(body), "authoredMergeRequests") {
		var res glAPIResult
		if strings.Contains(string(body), "mergeRequestInteraction") || strings.Contains(string(body), "mergedAt") {
			res = glAPIResult{Status: http.StatusOK, Body: []byte(`{"data":{"currentUser":null}}`)}
		} else {
			res = glAPIResult{Status: http.StatusOK, Body: []byte(`{"data":{"currentUser":{"username":"jdoe","authoredMergeRequests":{"count":1,"nodes":[` +
				`{"iid":"7","title":"Mine","webUrl":"` + testMRBase + `/acme/mine/-/merge_requests/7"}]}}}}`)}
		}
		res.Header = http.Header{}
		return res, nil
	}
	switch g.projectStatus {
	case 0:
	case http.StatusTooManyRequests:
		return glAPIResult{Status: g.projectStatus, Header: http.Header{"Retry-After": {"90"}}}, nil
	default:
		return glAPIResult{Status: g.projectStatus, Body: []byte(`{"message":"bad gateway"}`), Header: http.Header{}}, nil
	}
	var payload struct {
		Variables map[string]string `json:"variables"`
	}
	_ = json.Unmarshal(body, &payload)
	var read []string
	data := map[string]any{}
	for i := range len(payload.Variables) {
		alias := "p" + strconv.Itoa(i)
		path := payload.Variables[alias]
		read = append(read, path)
		// GitLab finds a project whatever the case it is asked in, and
		// answers with its own spelling.
		full := map[string]string{"acme/mine": "acme/mine", "acme/allowed": "Acme/Allowed"}[strings.ToLower(path)]
		if strings.HasPrefix(path, "many/") {
			full = path
		}
		if full == "" {
			data[alias] = nil
			continue
		}
		data[alias] = map[string]any{"fullPath": full, "mergeRequests": map[string]any{"count": 1, "nodes": []any{map[string]any{
			"iid": "3", "title": "In " + full, "webUrl": testMRBase + "/" + full + "/-/merge_requests/3",
			"author": map[string]any{"username": "ann"},
		}}}}
	}
	g.projectRead = append(g.projectRead, read)
	out, _ := json.Marshal(map[string]any{"data": data})
	return glAPIResult{Status: http.StatusOK, Body: out, Header: http.Header{}}, nil
}

func scopedWorld(t *testing.T, allowed ...string) *scopedGitLab {
	t.Helper()
	setupTestDB(t)
	tokenPath := filepath.Join(t.TempDir(), "gitlab-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte("glpat-scoped\n"), 0o600))
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{GitProxy: &config.GitProxyConfig{
		AllowedRemotes:  allowed,
		GitLabHosts:     map[string]string{"gitlab.example.test": testMRBase},
		GitLabTokenFile: tokenPath,
	}}}))
	t.Cleanup(func() { resetAuthoredOpenMRs("") })
	authoredOpenMRAgentProjects.Lock()
	authoredOpenMRAgentProjects.set = nil
	authoredOpenMRAgentProjects.Unlock()
	gl := &scopedGitLab{listStatus: http.StatusForbidden}
	t.Cleanup(SetGitLabTransportForTest(gl.do))
	return gl
}

func TestPollAuthoredOpenMRs_ARefusedInstanceListIsReadProjectByProject(t *testing.T) {
	gl := scopedWorld(t, "gitlab.example.test/acme/allowed", "gitlab.example.test/acme/*", "gitlab.example.test/acme/group")

	_, err := pollAuthoredOpenMRs(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, gl.listCalls)
	require.Len(t, gl.projectRead, 1)
	assert.Equal(t, []string{"acme/mine", "acme/allowed", "acme/group"}, gl.projectRead[0],
		"the Mine project and the exact allowed_remotes ones; the wildcard reads nothing")

	view := authoredOpenMRsSnapshot(loadCfg(t), nil)
	require.NotNil(t, view)
	require.Len(t, view.Instances, 1)
	assert.Empty(t, view.Instances[0].EveryoneError, "a refused instance-wide list alone is not an error")
	assert.Equal(t, 2, view.Instances[0].EveryoneByProject,
		"the popover says how many projects were read individually; a path GitLab has no project for does not count")
	assert.Equal(t, 2, view.EveryoneTotal)
	require.Len(t, view.Projects, 2)
	var names []string
	for _, p := range view.Projects {
		names = append(names, p.Project)
	}
	assert.ElementsMatch(t, []string{"acme/mine", "Acme/Allowed"}, names, "a project takes GitLab's own spelling")

	// The refusal is remembered: the next poll goes straight to the projects.
	_, err = pollAuthoredOpenMRs(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, gl.listCalls, "a refused list is not asked again every poll")
	assert.Len(t, gl.projectRead, 2)

	// Until everyoneOpenMRReprobe has passed; then it is asked again, and a
	// token that may now list the instance gets the instance-wide path.
	authoredOpenMRState.Lock()
	refusal := authoredOpenMRState.refused[testMRBase]
	refusal.at = refusal.at.Add(-everyoneOpenMRReprobe)
	authoredOpenMRState.refused[testMRBase] = refusal
	authoredOpenMRState.Unlock()
	gl.mu.Lock()
	gl.listStatus = http.StatusOK
	gl.mu.Unlock()
	_, err = pollAuthoredOpenMRs(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, gl.listCalls, "re-probed")
	assert.Equal(t, []string{"acme/listed"}, gl.projectRead[2], "discovery names the projects again")
	view = authoredOpenMRsSnapshot(loadCfg(t), nil)
	require.NotNil(t, view)
	assert.Zero(t, view.Instances[0].EveryoneByProject)
	authoredOpenMRState.RLock()
	_, still := authoredOpenMRState.refused[testMRBase]
	authoredOpenMRState.RUnlock()
	assert.False(t, still)
}

func TestPollAuthoredOpenMRs_ARefusedListWithNothingToReadIsAnError(t *testing.T) {
	gl := scopedWorld(t, "gitlab.example.test/acme/*")
	// No Mine merge request either: replace the GraphQL answer.
	t.Cleanup(SetGitLabTransportForTest(func(ctx context.Context, base, token string, req glAPIRequest) (glAPIResult, error) {
		body, _ := json.Marshal(req.Body)
		if req.GraphQL && strings.Contains(string(body), "authoredMergeRequests") && !strings.Contains(string(body), "mergedAt") &&
			!strings.Contains(string(body), "mergeRequestInteraction") {
			return graphQLAnswer(t, "jdoe", 0), nil
		}
		return gl.do(ctx, base, token, req)
	}))
	_, err := pollAuthoredOpenMRs(context.Background())
	require.NoError(t, err, "your own list answered")
	assert.Empty(t, gl.projectRead, "a project with no agent, no Mine merge request and only a wildcard is not read")
	view := authoredOpenMRsSnapshot(loadCfg(t), nil)
	require.NotNil(t, view)
	assert.Contains(t, view.Instances[0].EveryoneError, "insufficient_granular_scope")
	assert.Contains(t, view.Instances[0].EveryoneError, "no project is known")
	assert.Zero(t, view.Instances[0].EveryoneByProject)
}

func TestPollAuthoredOpenMRs_BothPathsFailingShowsBothReasons(t *testing.T) {
	gl := scopedWorld(t)
	gl.projectStatus = http.StatusBadGateway
	_, err := pollAuthoredOpenMRs(context.Background())
	require.NoError(t, err)
	view := authoredOpenMRsSnapshot(loadCfg(t), nil)
	require.NotNil(t, view)
	msg := view.Instances[0].EveryoneError
	assert.Contains(t, msg, "insufficient_granular_scope", "the instance-wide call's reason")
	assert.Contains(t, msg, "bad gateway", "and the fallback's")
	assert.Empty(t, view.Instances[0].Error, "your own list is fine")
}

func TestReadEveryoneOpenMRs_A401IsNotAFallback(t *testing.T) {
	gl := &scopedGitLab{listStatus: http.StatusUnauthorized}
	t.Cleanup(SetGitLabTransportForTest(gl.do))
	refused := map[string]everyoneRefusal{}
	_, _, err := readEveryoneOpenMRs(context.Background(), testMRBase, "glpat", nil, []string{"acme/mine"}, refused, time.Now())
	require.Error(t, err)
	assert.Empty(t, gl.projectRead, "a bad token is not read project by project")
	assert.Empty(t, refused)
}

func TestReadEveryoneOpenMRs_TheFallbackKeepsTheProjectCap(t *testing.T) {
	gl := &scopedGitLab{listStatus: http.StatusForbidden}
	t.Cleanup(SetGitLabTransportForTest(gl.do))
	var known []string
	for i := range everyoneOpenMRMaxProjects + 5 {
		known = append(known, "many/p"+strconv.Itoa(i))
	}
	got, _, err := readEveryoneOpenMRs(context.Background(), testMRBase, "glpat", nil, known, map[string]everyoneRefusal{}, time.Now())
	require.NoError(t, err)
	assert.True(t, got.truncated, "projects past the cap make all a floor")
	assert.Equal(t, everyoneOpenMRMaxProjects, got.byProject)
	assert.Len(t, gl.projectRead, everyoneOpenMRMaxProjects/everyoneOpenMRChunk, "five to a query")
}

func TestReadEveryoneOpenMRs_ARateLimitedFallbackHonoursTheWait(t *testing.T) {
	gl := &scopedGitLab{listStatus: http.StatusForbidden, projectStatus: http.StatusTooManyRequests}
	t.Cleanup(SetGitLabTransportForTest(gl.do))
	_, retry, err := readEveryoneOpenMRs(context.Background(), testMRBase, "glpat", nil, []string{"acme/mine"},
		map[string]everyoneRefusal{}, time.Now())
	require.Error(t, err)
	assert.Equal(t, 90*time.Second, retry)
	assert.Contains(t, err.Error(), "insufficient_granular_scope")
	assert.Contains(t, err.Error(), "rate limit")
}

func TestReadEveryoneOpenMRs_ATransientReprobeFailureKeepsReadingProjects(t *testing.T) {
	gl := &scopedGitLab{listStatus: http.StatusBadGateway}
	t.Cleanup(SetGitLabTransportForTest(gl.do))
	now := time.Now()
	refused := map[string]everyoneRefusal{testMRBase: {at: now.Add(-everyoneOpenMRReprobe), reason: "insufficient_granular_scope"}}
	got, _, err := readEveryoneOpenMRs(context.Background(), testMRBase, "glpat", nil, []string{"acme/mine"}, refused, now)
	require.NoError(t, err, "a 5xx on the re-probe says nothing new about the token")
	assert.Equal(t, 1, gl.listCalls)
	assert.Equal(t, 1, got.byProject)
	assert.Equal(t, now, refused[testMRBase].at, "and the next re-probe waits its interval")
	assert.Equal(t, "insufficient_granular_scope", refused[testMRBase].reason)
}

func TestPollAuthoredOpenMRs_ANewTokenAsksTheInstanceWideListAgain(t *testing.T) {
	gl := scopedWorld(t)
	_, err := pollAuthoredOpenMRs(context.Background())
	require.NoError(t, err)
	_, err = pollAuthoredOpenMRs(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, gl.listCalls, "remembered under the same token")

	policy := loadCfg(t).ResolvedGitProxy()
	require.NoError(t, os.WriteFile(policy.GitLabTokenFile, []byte("glpat-wider\n"), 0o600))
	_, err = pollAuthoredOpenMRs(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, gl.listCalls, "a refusal recorded for one token is not held against another")
}

func TestPollAuthoredOpenMRs_AnAgentsProjectIsReadFirstInTheFallback(t *testing.T) {
	gl := scopedWorld(t, "gitlab.example.test/acme/allowed")
	agents := []dashboardAgent{{AgentID: "agt_a", repoLinksView: repoLinksView{BranchURL: testMRBase + "/Acme/Allowed/-/tree/topic"}}}
	// Every snapshot records the agents' projects for the poller.
	openMRProjectsSnapshot(nil, agents)
	_, err := pollAuthoredOpenMRs(context.Background())
	require.NoError(t, err)
	require.Len(t, gl.projectRead, 1)
	assert.Equal(t, []string{"acme/allowed", "acme/mine"}, gl.projectRead[0],
		"the agent's project first, and not again for its allowed_remotes entry")
	view := authoredOpenMRsSnapshot(loadCfg(t), agents)
	require.NotNil(t, view)
	require.NotEmpty(t, view.Projects)
	assert.Equal(t, "Acme/Allowed", view.Projects[0].Project)
	assert.True(t, view.Projects[0].Agents)
}

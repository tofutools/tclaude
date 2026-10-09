package agentd_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// gitlabproxy_flow_test.go drives the GitLab proxy through the real daemon
// mux with only the outbound-HTTP boundary stubbed, on the same terms as the
// GitHub suite. The assertions concentrate on what would fail silently:
//
//   - the PROJECT is derived from the agent's own allow-listed remote, as the
//     full nested path, and never from a request parameter;
//   - the allow-list admits exactly the projects the operator wrote down, so
//     a nested path the pattern does not cover is refused;
//   - a remote is only a GitLab project when its host is a configured
//     instance, and every request goes to that instance's configured base —
//     including one on a non-default port the SSH remote never mentions;
//   - the token travels as a header and nowhere else.

const (
	glTestConvRemote = "git@gitlab.yamzz.test:yamzz/sub/previewplayback.git"
	glTestBase       = "https://gitlab.yamzz.test:6080"
	glTestProjectID  = "yamzz%2Fsub%2Fpreviewplayback"
	glTestToken      = "glpat-test-token-from-file"
)

type glCanned struct {
	Status int
	Body   string
	Header http.Header
	Err    error
}

// glRecorder is the stubbed outbound edge for GitLab: every request the daemon
// built, the token and base it was sent with, and a script to answer from.
type glRecorder struct {
	mu     sync.Mutex
	calls  []agentd.GitLabRequestForTest
	tokens []string

	route func(req agentd.GitLabRequestForTest) (glCanned, bool)
	seq   []glCanned
	def   glCanned

	streams []agentd.GitLabRequestForTest
	bodies  map[string][]byte
}

func newGLRecorder() *glRecorder {
	return &glRecorder{def: glCanned{Status: 200, Body: "{}"}, bodies: map[string][]byte{}}
}

func (r *glRecorder) do(_ context.Context, token string, req agentd.GitLabRequestForTest) (int, []byte, http.Header, error) {
	r.mu.Lock()
	r.calls = append(r.calls, req)
	r.tokens = append(r.tokens, token)
	answer := r.def
	if r.route != nil {
		if got, ok := r.route(req); ok {
			answer = got
		}
	}
	if len(r.seq) > 0 {
		answer = r.seq[0]
		r.seq = r.seq[1:]
	}
	r.mu.Unlock()
	if answer.Err != nil {
		return 0, nil, nil, answer.Err
	}
	header := answer.Header
	if header == nil {
		header = http.Header{}
	}
	return answer.Status, []byte(answer.Body), header, nil
}

func (r *glRecorder) stream(_ context.Context, token string, req agentd.GitLabRequestForTest, dst func([]byte) error) (int, error) {
	r.mu.Lock()
	r.streams = append(r.streams, req)
	r.tokens = append(r.tokens, token)
	body, ok := r.bodies[req.Path]
	r.mu.Unlock()
	if !ok {
		return http.StatusNotFound, nil
	}
	if err := dst(body); err != nil {
		return 0, err
	}
	return http.StatusOK, nil
}

func (r *glRecorder) requests() []agentd.GitLabRequestForTest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]agentd.GitLabRequestForTest(nil), r.calls...)
}

func (r *glRecorder) streamed() []agentd.GitLabRequestForTest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]agentd.GitLabRequestForTest(nil), r.streams...)
}

func (r *glRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *glRecorder) only(t *testing.T) agentd.GitLabRequestForTest {
	t.Helper()
	calls := r.requests()
	require.Len(t, calls, 1, "expected exactly one GitLab call")
	return calls[0]
}

func glJSONBody(t *testing.T, req agentd.GitLabRequestForTest) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(req.Body, &out))
	return out
}

// glWorld is gitProxyWorld with the agent's remote pointed at a self-hosted
// GitLab instance on a non-default port, that instance configured under
// gitlab_hosts, a token file, and the GitLab transport stubbed.
func glWorld(t *testing.T, allowed []string) (*testharness.Flow, *gitProxyRecorder, *glRecorder) {
	t.Helper()
	f, git := gitProxyWorld(t, allowed)
	git.remotes["origin"] = glTestConvRemote
	tokenPath := filepath.Join(t.TempDir(), "gitlab-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte(glTestToken+"\n"), 0o600))
	writeGitProxyConfig(t, allowed, func(c *gitProxyConfigPatch) {
		c.GitLabHosts = map[string]string{"gitlab.yamzz.test": glTestBase}
		c.GitLabTokenFile = tokenPath
	})
	gl := newGLRecorder()
	t.Cleanup(agentd.SetGitLabTransportForTestJSON(gl.do))
	t.Cleanup(agentd.SetGitLabStreamForTestBytes(gl.stream))
	return f, git, gl
}

// glOutcome decodes the daemon's wire answer.
func glOutcome(t *testing.T, body []byte) (exit int, stdout, stderr string, doc json.RawMessage) {
	t.Helper()
	var out struct {
		ExitCode int             `json:"exit_code"`
		Stdout   string          `json:"stdout"`
		Stderr   string          `json:"stderr"`
		JSON     json.RawMessage `json:"json"`
	}
	require.NoError(t, json.Unmarshal(body, &out))
	return out.ExitCode, out.Stdout, out.Stderr, out.JSON
}

const glCannedMR = `{"iid":7,"title":"Add a thing","description":"why","state":"opened","draft":false,
"source_branch":"feat/thing","target_branch":"main","web_url":"https://gitlab.yamzz.test:6080/yamzz/sub/previewplayback/-/merge_requests/7",
"created_at":"2026-10-01T10:00:00Z","updated_at":"2026-10-02T10:00:00Z","sha":"abc123","has_conflicts":false,
"detailed_merge_status":"mergeable","author":{"id":3,"username":"agent-bot","name":"Agent Bot"},
"head_pipeline":{"id":99,"sha":"abc123","ref":"feat/thing","status":"failed","web_url":"https://gitlab.yamzz.test:6080/yamzz/sub/previewplayback/-/pipelines/99"}}`

// ---------------------------------------------------------------------------
// Permission gating and the allow-list
// ---------------------------------------------------------------------------

func TestGLProxy_WriteRequiresItsOwnSlug(t *testing.T) {
	t.Run("proxy.gitlab.read does not confer proxy.gitlab.write", func(t *testing.T) {
		f, git, gl := glWorld(t, []string{"gitlab.yamzz.test/yamzz"})
		require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitLabRead, "test"))

		res := gitProxyPost(t, f, "/v1/gitlab/mr/create", map[string]any{"title": "Add a thing", "base": "main"})
		assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		assert.Equal(t, 0, gl.count(), "a denied caller spends no credential")
		assert.False(t, git.sawAnySubprocess())
	})

	t.Run("granted", func(t *testing.T) {
		f, _, gl := glWorld(t, []string{"gitlab.yamzz.test/yamzz"})
		gl.def = glCanned{Status: 201, Body: glCannedMR}
		require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitLabWrite, "test"))

		res := gitProxyPost(t, f, "/v1/gitlab/mr/create", map[string]any{"title": "Add a thing", "body": "why", "base": "main"})
		require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())

		call := gl.only(t)
		assert.Equal(t, glTestBase, call.Base, "the request goes to the configured instance, port included")
		assert.Equal(t, http.MethodPost, call.Method)
		assert.Equal(t, "projects/"+glTestProjectID+"/merge_requests", call.Path,
			"the project is the full nested path, escaped as one segment")
		body := glJSONBody(t, call)
		assert.Equal(t, "feat/thing", body["source_branch"], "the head is the agent's own branch, read by the daemon")
		assert.Equal(t, "main", body["target_branch"])
		assert.Equal(t, "Add a thing", body["title"])

		_, stdout, _, _ := glOutcome(t, res.Body.Bytes())
		assert.Contains(t, stdout, "/-/merge_requests/7")
		agentID, err := db.AgentIDForConv(gitProxyTestConv)
		require.NoError(t, err)
		presented, err := db.GetAgentPR(agentID, "https://gitlab.yamzz.test:6080/yamzz/sub/previewplayback/-/merge_requests/7")
		require.NoError(t, err)
		assert.NotEmpty(t, presented.ValidatedRepoRoot, "a created MR is recorded like a created PR")
		assert.Equal(t, "open", presented.State)
	})
}

// TestGLProxy_OnlyAllowListedProjects — "authorize certain repos, not all of
// them": the operator's pattern decides, segment by segment, and a nested
// project path the pattern does not reach is refused before any call.
func TestGLProxy_OnlyAllowListedProjects(t *testing.T) {
	cases := []struct {
		name    string
		allowed []string
		want    int
	}{
		{"the exact project", []string{"gitlab.yamzz.test/yamzz/sub/previewplayback"}, http.StatusOK},
		{"its group", []string{"gitlab.yamzz.test/yamzz"}, http.StatusOK},
		{"a wildcard subgroup", []string{"gitlab.yamzz.test/yamzz/*/previewplayback"}, http.StatusOK},
		{"a sibling project only", []string{"gitlab.yamzz.test/yamzz/sub/other"}, http.StatusForbidden},
		{"the same name outside the subgroup", []string{"gitlab.yamzz.test/yamzz/previewplayback"}, http.StatusForbidden},
		{"another instance", []string{"gitlab.com/yamzz"}, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, _, gl := glWorld(t, tc.allowed)
			gl.def = glCanned{Status: 200, Body: "[]"}
			require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitLabRead, "test"))

			res := gitProxyPost(t, f, "/v1/gitlab/mr/list", map[string]any{})
			assert.Equal(t, tc.want, res.Code, "body=%s", res.Body.String())
			if tc.want != http.StatusOK {
				assert.Equal(t, 0, gl.count(), "a refused remote spends no credential")
			}
		})
	}
}

func TestGLProxy_RemoteScopedRead(t *testing.T) {
	t.Run("matching scope passes", func(t *testing.T) {
		f, _, gl := glWorld(t, []string{"gitlab.yamzz.test"})
		gl.def = glCanned{Status: 200, Body: "[]"}
		require.NoError(t, db.GrantAgentPermissionWithScope(gitProxyTestConv, agentd.PermGitLabRead,
			`{"remote":["gitlab.yamzz.test/yamzz/*/*"]}`, "test"))
		res := gitProxyPost(t, f, "/v1/gitlab/mr/list", map[string]any{})
		require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
		assert.Equal(t, 1, gl.count())
	})
	t.Run("another project on the same instance is refused", func(t *testing.T) {
		f, git, gl := glWorld(t, []string{"gitlab.yamzz.test"})
		git.remotes["origin"] = "git@gitlab.yamzz.test:other/repo.git"
		require.NoError(t, db.GrantAgentPermissionWithScope(gitProxyTestConv, agentd.PermGitLabRead,
			`{"remote":["gitlab.yamzz.test/yamzz/*/*"]}`, "test"))
		res := gitProxyPost(t, f, "/v1/gitlab/mr/list", map[string]any{})
		assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		assert.Equal(t, 0, gl.count())
	})
}

// TestGLProxy_HostMustBeAConfiguredInstance — the allow-list admitting a remote
// is not enough to call it GitLab: a self-hosted instance is only recognised
// through gitlab_hosts, and a GitHub remote is pointed at the other command.
func TestGLProxy_HostMustBeAConfiguredInstance(t *testing.T) {
	t.Run("an allow-listed host with no gitlab_hosts entry", func(t *testing.T) {
		f, _, gl := glWorld(t, []string{"gitlab.yamzz.test"})
		writeGitProxyConfig(t, []string{"gitlab.yamzz.test"}, func(c *gitProxyConfigPatch) {
			c.GitLabTokenFile = "/nonexistent" // never reached: the host gate comes first
		})
		require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitLabRead, "test"))
		res := gitProxyPost(t, f, "/v1/gitlab/mr/list", map[string]any{})
		assert.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
		assert.Contains(t, res.Body.String(), "not_gitlab")
		assert.Contains(t, res.Body.String(), "gitlab_hosts")
		assert.Equal(t, 0, gl.count())
	})
	t.Run("a github remote", func(t *testing.T) {
		f, git, gl := glWorld(t, []string{"github.com", "gitlab.yamzz.test"})
		git.remotes["origin"] = gitProxyTestRemote
		require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitLabRead, "test"))
		res := gitProxyPost(t, f, "/v1/gitlab/mr/list", map[string]any{})
		assert.Equal(t, http.StatusConflict, res.Code, "body=%s", res.Body.String())
		assert.Contains(t, res.Body.String(), "proxy github")
		assert.Equal(t, 0, gl.count())
	})
	t.Run("no token file configured", func(t *testing.T) {
		f, _, gl := glWorld(t, []string{"gitlab.yamzz.test"})
		writeGitProxyConfig(t, []string{"gitlab.yamzz.test"}, func(c *gitProxyConfigPatch) {
			c.GitLabHosts = map[string]string{"gitlab.yamzz.test": glTestBase}
		})
		require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitLabRead, "test"))
		res := gitProxyPost(t, f, "/v1/gitlab/mr/list", map[string]any{})
		assert.Equal(t, http.StatusServiceUnavailable, res.Code, "body=%s", res.Body.String())
		assert.Contains(t, res.Body.String(), "gitlab_token_file")
		assert.Equal(t, 0, gl.count())
	})
}

// TestGLProxy_TokenTravelsInAHeaderAndNowhereElse — the token reaches the
// transport as a value and never a path, query or body.
func TestGLProxy_TokenTravelsInAHeaderAndNowhereElse(t *testing.T) {
	f, _, gl := glWorld(t, []string{"gitlab.yamzz.test"})
	gl.def = glCanned{Status: 200, Body: "[]"}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitLabRead, "test"))

	res := gitProxyPost(t, f, "/v1/gitlab/mr/list", map[string]any{})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	call := gl.only(t)
	gl.mu.Lock()
	tokens := append([]string(nil), gl.tokens...)
	gl.mu.Unlock()
	assert.Equal(t, []string{glTestToken}, tokens, "the token from the file, trimmed")
	assert.NotContains(t, call.Path, glTestToken)
	assert.NotContains(t, call.Query.Encode(), glTestToken)
	assert.NotContains(t, string(call.Body), glTestToken)
	assert.NotContains(t, res.Body.String(), glTestToken, "and it is never rendered back")
}

// TestGLProxy_ProjectIsDerivedNotNamed — there is no project parameter.
func TestGLProxy_ProjectIsDerivedNotNamed(t *testing.T) {
	f, _, gl := glWorld(t, []string{"gitlab.yamzz.test"})
	gl.def = glCanned{Status: 200, Body: "[]"}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitLabRead, "test"))

	res := gitProxyPost(t, f, "/v1/gitlab/mr/list", map[string]any{
		"project": "attacker/exfil", "project_id": 1, "state": "open",
	})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	call := gl.only(t)
	assert.True(t, strings.HasPrefix(call.Path, "projects/"+glTestProjectID+"/"), "path=%s", call.Path)
	assert.NotContains(t, call.Path+call.Query.Encode()+string(call.Body), "attacker")
}

func TestGLProxy_ReadsArePostOnly(t *testing.T) {
	f, _, _ := glWorld(t, []string{"gitlab.yamzz.test"})
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitLabRead, "test"))
	res := serveAsProxyAgent(t, f, http.MethodGet, "/v1/gitlab/mr/list", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, res.Code)
}

// ---------------------------------------------------------------------------
// Merge requests
// ---------------------------------------------------------------------------

func TestGLProxy_MRListTranslatesTheFilters(t *testing.T) {
	f, _, gl := glWorld(t, []string{"gitlab.yamzz.test"})
	gl.def = glCanned{Status: 200, Body: "[" + glCannedMR + "]"}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitLabRead, "test"))

	res := gitProxyPost(t, f, "/v1/gitlab/mr/list", map[string]any{"state": "open", "head": "feat/thing", "limit": 5})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	call := gl.only(t)
	assert.Equal(t, "opened", call.Query.Get("state"), "the CLI's 'open' is GitLab's 'opened'")
	assert.Equal(t, "feat/thing", call.Query.Get("source_branch"))
	assert.Equal(t, "5", call.Query.Get("per_page"))

	_, _, _, doc := glOutcome(t, res.Body.Bytes())
	var rows []map[string]any
	require.NoError(t, json.Unmarshal(doc, &rows))
	require.Len(t, rows, 1)
	assert.EqualValues(t, 7, rows[0]["number"], "the iid is the number")
	assert.Equal(t, "feat/thing", rows[0]["headRefName"])
	assert.Equal(t, "agent-bot", rows[0]["author"].(map[string]any)["login"])
}

func TestGLProxy_MRListRefusesAnUnknownState(t *testing.T) {
	f, _, gl := glWorld(t, []string{"gitlab.yamzz.test"})
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitLabRead, "test"))
	res := gitProxyPost(t, f, "/v1/gitlab/mr/list", map[string]any{"state": "locked"})
	assert.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, 0, gl.count())
}

func TestGLProxy_MRViewProjectsGitLabFields(t *testing.T) {
	f, _, gl := glWorld(t, []string{"gitlab.yamzz.test"})
	gl.def = glCanned{Status: 200, Body: glCannedMR}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitLabRead, "test"))

	res := gitProxyPost(t, f, "/v1/gitlab/mr/view", map[string]any{"number": 7})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, "projects/"+glTestProjectID+"/merge_requests/7", gl.only(t).Path)
	_, _, _, doc := glOutcome(t, res.Body.Bytes())
	var view map[string]any
	require.NoError(t, json.Unmarshal(doc, &view))
	assert.Equal(t, "opened", view["state"])
	assert.Equal(t, true, view["mergeable"])
	assert.Equal(t, "mergeable", view["mergeStatus"])
	assert.EqualValues(t, 99, view["headPipeline"].(map[string]any)["id"])
}

func TestGLProxy_MRChecksReadsTheHeadPipelinesJobs(t *testing.T) {
	f, _, gl := glWorld(t, []string{"gitlab.yamzz.test"})
	gl.route = func(req agentd.GitLabRequestForTest) (glCanned, bool) {
		switch req.Path {
		case "projects/" + glTestProjectID + "/merge_requests/7":
			return glCanned{Status: 200, Body: glCannedMR}, true
		case "projects/" + glTestProjectID + "/pipelines/99/jobs":
			return glCanned{Status: 200, Body: `[{"id":501,"name":"build","stage":"build","status":"success"},
				{"id":502,"name":"test","stage":"test","status":"failed","allow_failure":false,"web_url":"https://gitlab.yamzz.test:6080/j/502"}]`}, true
		}
		return glCanned{}, false
	}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitLabRead, "test"))

	res := gitProxyPost(t, f, "/v1/gitlab/mr/checks", map[string]any{"number": 7})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	_, _, _, doc := glOutcome(t, res.Body.Bytes())
	var out struct {
		Number   int `json:"number"`
		Pipeline struct {
			ID     int64  `json:"id"`
			Status string `json:"status"`
		} `json:"pipeline"`
		Jobs []struct {
			ID     int64  `json:"id"`
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"jobs"`
	}
	require.NoError(t, json.Unmarshal(doc, &out))
	assert.Equal(t, 7, out.Number)
	assert.EqualValues(t, 99, out.Pipeline.ID)
	assert.Equal(t, "failed", out.Pipeline.Status)
	require.Len(t, out.Jobs, 2)
	assert.Equal(t, "test", out.Jobs[1].Name)
	assert.Equal(t, "failed", out.Jobs[1].Status)
}

func TestGLProxy_MRCreateDraftIsATitleMarker(t *testing.T) {
	f, _, gl := glWorld(t, []string{"gitlab.yamzz.test"})
	gl.def = glCanned{Status: 201, Body: strings.Replace(glCannedMR, `"title":"Add a thing"`, `"title":"Draft: Add a thing","draft":true`, 1)}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitLabWrite, "test"))

	res := gitProxyPost(t, f, "/v1/gitlab/mr/create", map[string]any{"title": "Add a thing", "base": "main", "draft": true})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	assert.Equal(t, "Draft: Add a thing", glJSONBody(t, gl.only(t))["title"])
	agentID, err := db.AgentIDForConv(gitProxyTestConv)
	require.NoError(t, err)
	presented, err := db.GetAgentPR(agentID, "https://gitlab.yamzz.test:6080/yamzz/sub/previewplayback/-/merge_requests/7")
	require.NoError(t, err)
	assert.Equal(t, "draft", presented.State)
	assert.Equal(t, "Add a thing", presented.Summary, "the recorded title loses the marker")
}

func TestGLProxy_MRCreateResolvesTheDefaultBranch(t *testing.T) {
	f, _, gl := glWorld(t, []string{"gitlab.yamzz.test"})
	gl.route = func(req agentd.GitLabRequestForTest) (glCanned, bool) {
		if req.Path == "projects/"+glTestProjectID {
			return glCanned{Status: 200, Body: `{"default_branch":"integration"}`}, true
		}
		return glCanned{Status: 201, Body: glCannedMR}, true
	}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitLabWrite, "test"))

	res := gitProxyPost(t, f, "/v1/gitlab/mr/create", map[string]any{"title": "Add a thing"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	calls := gl.requests()
	require.Len(t, calls, 2)
	assert.Equal(t, "integration", glJSONBody(t, calls[1])["target_branch"])
}

func TestGLProxy_MergeRequiresItsOwnSlugAndNamesTheCase(t *testing.T) {
	t.Run("proxy.gitlab.write does not confer merge", func(t *testing.T) {
		f, _, gl := glWorld(t, []string{"gitlab.yamzz.test"})
		require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitLabWrite, "test"))
		res := gitProxyPost(t, f, "/v1/gitlab/mr/merge", map[string]any{"number": 7})
		assert.Equal(t, http.StatusForbidden, res.Code, "body=%s", res.Body.String())
		assert.Equal(t, 0, gl.count())
	})
	t.Run("a draft is refused before the merge call", func(t *testing.T) {
		f, _, gl := glWorld(t, []string{"gitlab.yamzz.test"})
		gl.def = glCanned{Status: 200, Body: strings.Replace(glCannedMR, `"draft":false`, `"draft":true`, 1)}
		require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitLabMerge, "test"))
		res := gitProxyPost(t, f, "/v1/gitlab/mr/merge", map[string]any{"number": 7})
		require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
		exit, _, stderr, _ := glOutcome(t, res.Body.Bytes())
		assert.Equal(t, 1, exit)
		assert.Contains(t, stderr, "draft")
		assert.Equal(t, 1, gl.count(), "the state read only; nothing was merged")
	})
	t.Run("squash merge", func(t *testing.T) {
		f, _, gl := glWorld(t, []string{"gitlab.yamzz.test"})
		gl.route = func(req agentd.GitLabRequestForTest) (glCanned, bool) {
			if req.Method == http.MethodPut {
				return glCanned{Status: 200, Body: strings.Replace(glCannedMR, `"state":"opened"`, `"state":"merged","squash_commit_sha":"feedface"`, 1)}, true
			}
			return glCanned{Status: 200, Body: glCannedMR}, true
		}
		require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitLabMerge, "test"))
		res := gitProxyPost(t, f, "/v1/gitlab/mr/merge", map[string]any{"number": 7, "method": "squash", "subject": "Add a thing"})
		require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
		calls := gl.requests()
		require.Len(t, calls, 2)
		assert.Equal(t, "projects/"+glTestProjectID+"/merge_requests/7/merge", calls[1].Path)
		body := glJSONBody(t, calls[1])
		assert.Equal(t, true, body["squash"])
		assert.Equal(t, "Add a thing", body["squash_commit_message"])
		exit, stdout, _, _ := glOutcome(t, res.Body.Bytes())
		assert.Equal(t, 0, exit)
		assert.Contains(t, stdout, "merged !7 into main as feedface")
	})
	t.Run("rebase is not a merge method here", func(t *testing.T) {
		f, _, gl := glWorld(t, []string{"gitlab.yamzz.test"})
		require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitLabMerge, "test"))
		res := gitProxyPost(t, f, "/v1/gitlab/mr/merge", map[string]any{"number": 7, "method": "rebase"})
		assert.Equal(t, http.StatusBadRequest, res.Code, "body=%s", res.Body.String())
		assert.Equal(t, 0, gl.count())
	})
}

func TestGLProxy_MRReadyStripsTheMarker(t *testing.T) {
	f, _, gl := glWorld(t, []string{"gitlab.yamzz.test"})
	gl.route = func(req agentd.GitLabRequestForTest) (glCanned, bool) {
		if req.Method == http.MethodPut {
			return glCanned{Status: 200, Body: glCannedMR}, true
		}
		return glCanned{Status: 200, Body: strings.Replace(glCannedMR, `"title":"Add a thing","description":"why","state":"opened","draft":false`,
			`"title":"Draft: Add a thing","description":"why","state":"opened","draft":true`, 1)}, true
	}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitLabWrite, "test"))
	res := gitProxyPost(t, f, "/v1/gitlab/mr/ready", map[string]any{"number": 7})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	calls := gl.requests()
	require.Len(t, calls, 2)
	assert.Equal(t, "Add a thing", glJSONBody(t, calls[1])["title"])
}

func TestGLProxy_MRCommentsSplitsInlineNotes(t *testing.T) {
	f, _, gl := glWorld(t, []string{"gitlab.yamzz.test"})
	gl.def = glCanned{Status: 200, Body: `[
		{"id":1,"body":"added 2 commits","system":true,"author":{"username":"agent-bot"},"created_at":"2026-10-01T10:00:00Z","updated_at":"2026-10-01T10:00:00Z"},
		{"id":2,"body":"Looks good overall","system":false,"author":{"username":"martin"},"created_at":"2026-10-01T11:00:00Z","updated_at":"2026-10-01T11:00:00Z"},
		{"id":3,"body":"this pointer is never initialised","system":false,"type":"DiffNote","resolvable":true,"resolved":false,
		 "author":{"username":"martin"},"created_at":"2026-10-01T11:05:00Z","updated_at":"2026-10-01T11:05:00Z",
		 "position":{"new_path":"src/Audio.cpp","new_line":42}}
	]`}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitLabRead, "test"))
	res := gitProxyPost(t, f, "/v1/gitlab/mr/comments", map[string]any{"number": 7})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	_, stdout, _, _ := glOutcome(t, res.Body.Bytes())
	assert.Contains(t, stdout, "Looks good overall")
	assert.Contains(t, stdout, "file:\tsrc/Audio.cpp:42")
	assert.Contains(t, stdout, "this pointer is never initialised")
	assert.Contains(t, stdout, "#note_3")
	assert.NotContains(t, stdout, "added 2 commits", "system notes are bookkeeping, not feedback")
	call := gl.only(t)
	assert.Equal(t, "asc", call.Query.Get("sort"))
}

func TestGLProxy_CommentPostsANote(t *testing.T) {
	f, _, gl := glWorld(t, []string{"gitlab.yamzz.test"})
	gl.def = glCanned{Status: 201, Body: `{"id":77,"body":"thanks"}`}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitLabWrite, "test"))
	res := gitProxyPost(t, f, "/v1/gitlab/issue/comment", map[string]any{"number": 12, "body": "thanks"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	call := gl.only(t)
	assert.Equal(t, "projects/"+glTestProjectID+"/issues/12/notes", call.Path)
	assert.Equal(t, "thanks", glJSONBody(t, call)["body"])
	_, stdout, _, _ := glOutcome(t, res.Body.Bytes())
	assert.Equal(t, glTestBase+"/yamzz/sub/previewplayback/-/issues/12#note_77\n", stdout)
}

func TestGLProxy_GitLabErrorsReachTheAgentVerbatim(t *testing.T) {
	f, _, gl := glWorld(t, []string{"gitlab.yamzz.test"})
	gl.def = glCanned{Status: 400, Body: `{"message":{"title":["can't be blank"],"source_branch":["is invalid"]}}`}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitLabWrite, "test"))
	res := gitProxyPost(t, f, "/v1/gitlab/mr/edit", map[string]any{"number": 7, "title": "x"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	exit, _, stderr, _ := glOutcome(t, res.Body.Bytes())
	assert.Equal(t, 1, exit)
	assert.Contains(t, stderr, "source_branch: is invalid")
	assert.Contains(t, stderr, "title: can't be blank")
}

// ---------------------------------------------------------------------------
// Pipelines and jobs
// ---------------------------------------------------------------------------

func TestGLProxy_PipelineListFilters(t *testing.T) {
	f, _, gl := glWorld(t, []string{"gitlab.yamzz.test"})
	gl.def = glCanned{Status: 200, Body: `[{"id":99,"sha":"abc","ref":"feat/thing","status":"failed","web_url":"u"}]`}
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitLabRead, "test"))

	res := gitProxyPost(t, f, "/v1/gitlab/pipeline/list", map[string]any{"branch": "feat/thing", "status": "failed"})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	call := gl.only(t)
	assert.Equal(t, "projects/"+glTestProjectID+"/pipelines", call.Path)
	assert.Equal(t, "feat/thing", call.Query.Get("ref"))
	assert.Equal(t, "failed", call.Query.Get("status"))

	res = gitProxyPost(t, f, "/v1/gitlab/pipeline/list", map[string]any{"status": "exploded"})
	assert.Equal(t, http.StatusBadRequest, res.Code)
}

func TestGLProxy_JobLogFailedReadsOnlyTheFailedTraces(t *testing.T) {
	f, _, gl := glWorld(t, []string{"gitlab.yamzz.test"})
	gl.def = glCanned{Status: 200, Body: `[{"id":501,"name":"build","stage":"build","status":"success"},
		{"id":502,"name":"test","stage":"test","status":"failed"},
		{"id":503,"name":"lint","stage":"test","status":"failed","allow_failure":true}]`}
	gl.bodies["projects/"+glTestProjectID+"/jobs/502/trace"] = []byte("running tests\nFAIL: tst_sessioncontroller\n")
	gl.bodies["projects/"+glTestProjectID+"/jobs/503/trace"] = []byte("warning: unused variable\n")
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitLabRead, "test"))

	res := gitProxyPost(t, f, "/v1/gitlab/job/log-failed", map[string]any{"pipeline_id": 99})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	_, stdout, _, _ := glOutcome(t, res.Body.Bytes())
	assert.Contains(t, stdout, "test\ttest\tFAIL: tst_sessioncontroller")
	assert.Contains(t, stdout, "lint\ttest (allowed to fail)\twarning: unused variable")
	streamed := gl.streamed()
	require.Len(t, streamed, 2, "the green job's trace is not fetched")
	assert.Equal(t, "text/plain", streamed[0].Accept)
}

func TestGLProxy_JobDownloadLandsInTheWorkTree(t *testing.T) {
	f, git, gl := glWorld(t, []string{"gitlab.yamzz.test"})
	gl.def = glCanned{Status: 200, Body: `{"id":502,"name":"test","stage":"test","status":"failed",
		"artifacts_file":{"filename":"artifacts.zip","size":123},"artifacts":[{"file_type":"archive","filename":"artifacts.zip","size":123}]}`}
	var archive bytes.Buffer
	zw := zip.NewWriter(&archive)
	w, err := zw.Create("report/junit.xml")
	require.NoError(t, err)
	_, err = w.Write([]byte("<testsuite/>"))
	require.NoError(t, err)
	require.NoError(t, zw.Close())
	gl.bodies["projects/"+glTestProjectID+"/jobs/502/artifacts"] = archive.Bytes()
	require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitLabRead, "test"))

	res := gitProxyPost(t, f, "/v1/gitlab/job/download", map[string]any{"job_id": 502})
	require.Equal(t, http.StatusOK, res.Code, "body=%s", res.Body.String())
	dest := filepath.Join(git.repoRoot, ".tclaude-artifacts", "job-502")
	assert.FileExists(t, filepath.Join(dest, "report", "junit.xml"))
	_, stdout, _, _ := glOutcome(t, res.Body.Bytes())
	assert.Contains(t, stdout, "report/junit.xml")
	assert.FileExists(t, filepath.Join(git.repoRoot, ".tclaude-artifacts", ".gitignore"))
}

func TestGLProxy_JobDownloadRefusesAnExpiredOrOversizedArchive(t *testing.T) {
	t.Run("expired", func(t *testing.T) {
		f, _, gl := glWorld(t, []string{"gitlab.yamzz.test"})
		gl.def = glCanned{Status: 200, Body: `{"id":502,"name":"test","status":"failed",
			"artifacts_file":{"filename":"artifacts.zip","size":5},"artifacts_expire_at":"2020-01-01T00:00:00Z"}`}
		require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitLabRead, "test"))
		res := gitProxyPost(t, f, "/v1/gitlab/job/download", map[string]any{"job_id": 502})
		assert.Equal(t, http.StatusNotFound, res.Code, "body=%s", res.Body.String())
		assert.Contains(t, res.Body.String(), "artifact_expired")
		assert.Empty(t, gl.streamed())
	})
	t.Run("oversized", func(t *testing.T) {
		f, _, gl := glWorld(t, []string{"gitlab.yamzz.test"})
		gl.def = glCanned{Status: 200, Body: `{"id":502,"name":"test","status":"failed",
			"artifacts_file":{"filename":"artifacts.zip","size":9999999999}}`}
		require.NoError(t, db.GrantAgentPermission(gitProxyTestConv, agentd.PermGitLabRead, "test"))
		res := gitProxyPost(t, f, "/v1/gitlab/job/download", map[string]any{"job_id": 502})
		assert.Equal(t, http.StatusRequestEntityTooLarge, res.Code, "body=%s", res.Body.String())
		assert.Empty(t, gl.streamed())
	})
}

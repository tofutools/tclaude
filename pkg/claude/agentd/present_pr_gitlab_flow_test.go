package agentd_test

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// present_pr_gitlab_flow_test.go: a merge request presented in hardened mode
// is validated against the caller's own GitLab remote, rendered on the
// dashboard with its iid, and refreshed through the GitLab API rather than
// through `gh`.

const (
	presentMRTestRemote = "git@gitlab.contoso.test:contoso/sub/player.git"
	presentMRTestURL    = "https://gitlab.contoso.test:8929/contoso/sub/player/-/merge_requests/157"
)

// saveGitLabPresentedPRPolicy writes a policy that recognises the test
// instance and holds a token for it.
func saveGitLabPresentedPRPolicy(t *testing.T, allowed ...string) {
	t.Helper()
	tokenPath := filepath.Join(t.TempDir(), "gitlab-token")
	require.NoError(t, os.WriteFile(tokenPath, []byte(glTestToken+"\n"), 0o600))
	require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{GitProxy: &config.GitProxyConfig{
		AllowedRemotes:  allowed,
		GitLabHosts:     map[string]string{"gitlab.contoso.test": glTestBase},
		GitLabTokenFile: tokenPath,
	}}}))
}

func TestPresentPR_GitLabMergeRequestIsValidatedAgainstTheCallersRemote(t *testing.T) {
	t.Run("the caller's own project", func(t *testing.T) {
		f := newFlow(t)
		t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
		const worker = "ppgl-aaaa-bbbb-cccc-dddd"
		f.HaveGroup("alpha")
		f.HaveConvWithTitle(worker, "worker")
		repo := presentedPRTestRepo(t, f, "ppgl", presentMRTestRemote, "gitlab.contoso.test/contoso")
		saveGitLabPresentedPRPolicy(t, "gitlab.contoso.test/contoso")
		f.HaveAliveSession(worker, "lbl-ppgl", "tmux-ppgl", repo)
		f.HaveMember("alpha", worker)
		require.NoError(t, db.SetAgentPermissionOverride(worker, agentd.PermSelfPR, db.PermEffectGrant, "test"))

		rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
			testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/prs",
				map[string]any{"url": presentMRTestURL, "summary": "manual CTAD", "state": "open"}), worker))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		snap := fetchDashSnapshot(t, agentd.BuildDashboardHandlerForTest())
		m := findDashMember(snap, "alpha", worker)
		require.NotNil(t, m)
		require.Len(t, m.PresentedPRs, 1)
		assert.Equal(t, presentMRTestURL, m.PresentedPRs[0].URL)
		assert.Equal(t, 157, m.PresentedPRs[0].Number, "the pill shows the iid")

		agentID, err := db.AgentIDForConv(worker)
		require.NoError(t, err)
		row, err := db.GetAgentPR(agentID, presentMRTestURL)
		require.NoError(t, err)
		assert.NotEmpty(t, row.ValidatedRepoRoot)
	})

	t.Run("another project on the instance is refused", func(t *testing.T) {
		f := newFlow(t)
		const worker = "ppgm-aaaa-bbbb-cccc-dddd"
		f.HaveConvWithTitle(worker, "worker")
		repo := presentedPRTestRepo(t, f, "ppgm", presentMRTestRemote, "gitlab.contoso.test")
		saveGitLabPresentedPRPolicy(t, "gitlab.contoso.test")
		f.HaveAliveSession(worker, "lbl-ppgm", "tmux-ppgm", repo)
		require.NoError(t, db.SetAgentPermissionOverride(worker, agentd.PermSelfPR, db.PermEffectGrant, "test"))

		rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
			testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/prs",
				map[string]any{"url": "https://gitlab.contoso.test:8929/contoso/other/-/merge_requests/1"}), worker))
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "pr_repo_refused")
	})

	t.Run("an instance that is not configured is refused", func(t *testing.T) {
		f := newFlow(t)
		const worker = "ppgn-aaaa-bbbb-cccc-dddd"
		f.HaveConvWithTitle(worker, "worker")
		repo := presentedPRTestRepo(t, f, "ppgn", "git@gitlab.other.test:contoso/app.git", "gitlab.other.test")
		saveGitLabPresentedPRPolicy(t, "gitlab.other.test")
		f.HaveAliveSession(worker, "lbl-ppgn", "tmux-ppgn", repo)
		require.NoError(t, db.SetAgentPermissionOverride(worker, agentd.PermSelfPR, db.PermEffectGrant, "test"))

		rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
			testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/prs",
				map[string]any{"url": "https://gitlab.other.test/contoso/app/-/merge_requests/1"}), worker))
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "not a configured GitLab instance")
	})
}

func TestPresentPR_GitLabMergeRequestRefreshesThroughTheAPI(t *testing.T) {
	f := newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	gl := newGLRecorder()
	t.Cleanup(agentd.SetGitLabTransportForTestJSON(gl.do))
	gl.route = func(req agentd.GitLabRequestForTest) (glCanned, bool) {
		switch req.Path {
		case "projects/" + glTestProjectID + "/merge_requests/157":
			return glCanned{Status: 200, Body: `{"iid":157,"title":"Draft: Manual CTAD","state":"merged","draft":true,
				"web_url":"` + presentMRTestURL + `","head_pipeline":{"id":99,"status":"success","web_url":"` + glTestBase + `/p/99"}}`}, true
		case "projects/" + glTestProjectID + "/pipelines/99/jobs":
			return glCanned{Status: 200, Body: `[{"id":1,"name":"build","stage":"build","status":"success"},{"id":2,"name":"test","stage":"test","status":"success"}]`}, true
		}
		return glCanned{}, false
	}
	const worker = "ppgr-aaaa-bbbb-cccc-dddd"
	f.HaveGroup("alpha")
	f.HaveConvWithTitle(worker, "worker")
	repo := presentedPRTestRepo(t, f, "ppgr", presentMRTestRemote, "gitlab.contoso.test/contoso")
	saveGitLabPresentedPRPolicy(t, "gitlab.contoso.test/contoso")
	f.HaveAliveSession(worker, "lbl-ppgr", "tmux-ppgr", repo)
	f.HaveMember("alpha", worker)
	require.NoError(t, db.SetAgentPermissionOverride(worker, agentd.PermSelfPR, db.PermEffectGrant, "test"))

	rec := testharness.Serve(f.Mux, agentd.AsAgentPeer(
		testharness.JSONRequest(t, http.MethodPost, "/v1/whoami/prs",
			map[string]any{"url": presentMRTestURL, "summary": "manual CTAD", "state": "open"}), worker))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	agentID, err := db.AgentIDForConv(worker)
	require.NoError(t, err)
	agePresentedPR(t, agentID, presentMRTestURL, 5*time.Minute)

	mux := agentd.BuildDashboardHandlerForTest()
	_ = fetchDashSnapshot(t, mux)
	agentd.WaitForBackgroundForTest()

	snap := fetchDashSnapshot(t, mux)
	m := findDashMember(snap, "alpha", worker)
	require.NotNil(t, m)
	require.Len(t, m.PresentedPRs, 1)
	assert.Equal(t, "merged", m.PresentedPRs[0].State, "the state came from the GitLab API")
	require.NotNil(t, m.PresentedPRs[0].Checks, "the head pipeline's jobs are the checks")
	assert.Equal(t, "passing", m.PresentedPRs[0].Checks.State)
	assert.Equal(t, 2, m.PresentedPRs[0].Checks.Total)

	calls := gl.requests()
	require.GreaterOrEqual(t, len(calls), 2)
	for _, c := range calls {
		assert.Equal(t, glTestBase, c.Base, "every refresh call goes to the configured instance")
	}
	gl.mu.Lock()
	tokens := append([]string(nil), gl.tokens...)
	gl.mu.Unlock()
	for _, tok := range tokens {
		assert.Equal(t, glTestToken, tok)
	}
}

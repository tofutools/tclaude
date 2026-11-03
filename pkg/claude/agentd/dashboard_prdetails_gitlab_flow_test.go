package agentd_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/testharness"
)

// dashboard_prdetails_gitlab_flow_test.go: the PR link hover card for a GitLab
// merge request. GET /api/pr-details reads it through the GitLab API, under
// the gate the merge request's CI pill passes, and caches it per URL.

const glDetailsMR = `{"iid":157,"title":"Draft: Manual CTAD","description":"## Background / Purpose\n\nTemplate deduction picked the **wrong** overload.",
"state":"opened","draft":true,"source_branch":"feat/ctad","target_branch":"main","web_url":"` + presentMRTestURL + `",
"created_at":"2026-10-01T10:00:00Z","merged_at":null,"closed_at":null,"author":{"id":3,"username":"jdoe","name":"J. Doe"}}`

// glDetailsWorld is a configured instance whose REST read answers mr and whose
// GraphQL read answers graphql.
func glDetailsWorld(t *testing.T, graphql string) (http.Handler, *glRecorder) {
	t.Helper()
	_ = newFlow(t)
	t.Cleanup(agentd.SetPopupBaseURLForTest("http://127.0.0.1:0"))
	saveGitLabPresentedPRPolicy(t, "gitlab.contoso.test/contoso")
	gl := newGLRecorder()
	t.Cleanup(agentd.SetGitLabTransportForTestJSON(gl.do))
	gl.route = func(req agentd.GitLabRequestForTest) (glCanned, bool) {
		switch {
		case req.GraphQL:
			return glCanned{Status: http.StatusOK, Body: graphql}, true
		case req.Path == "projects/"+glTestProjectID+"/merge_requests/157":
			return glCanned{Status: http.StatusOK, Body: glDetailsMR}, true
		}
		return glCanned{Status: http.StatusNotFound, Body: `{"message":"404 Not Found"}`}, true
	}
	return agentd.BuildDashboardHandlerForTest(), gl
}

func TestDashboardPRDetails_GitLabMergeRequestCard(t *testing.T) {
	mux, gl := glDetailsWorld(t, `{"data":{"project":{"mergeRequest":{"commitCount":4,
		"diffStatsSummary":{"additions":120,"deletions":7,"fileCount":3}}}}}`)

	first := fetchPRDetails(t, mux, presentMRTestURL)
	assert.True(t, first.Refreshing, "the first hover schedules the read")
	agentd.WaitForBackgroundForTest()

	card := fetchPRDetails(t, mux, presentMRTestURL)
	require.True(t, card.Resolved)
	require.NotNil(t, card.Details)
	d := card.Details
	assert.Equal(t, "Manual CTAD", d.Title, "the draft marker is a state, not part of the title")
	assert.Equal(t, "draft", d.State)
	assert.Equal(t, "jdoe", d.Author)
	assert.Equal(t, "feat/ctad", d.HeadRef)
	assert.Equal(t, "main", d.BaseRef)
	assert.Equal(t, "Template deduction picked the wrong overload.", d.Excerpt)
	assert.Equal(t, 120, d.Additions)
	assert.Equal(t, 7, d.Deletions)
	assert.Equal(t, 3, d.ChangedFiles)
	assert.Equal(t, 4, d.Commits)
	assert.Equal(t, "2026-10-01T10:00:00Z", d.CreatedAt)
	assert.False(t, d.NoDiffstat)
	assert.False(t, card.Refreshing, "a fresh card spends no read")

	calls := gl.requests()
	require.Len(t, calls, 2, "one REST read and one GraphQL read, then the cache")
	for _, call := range calls {
		assert.Equal(t, glTestBase, call.Base)
	}
	assert.Contains(t, string(calls[1].Body), `"project":"contoso/sub/player"`, "the project travels as a variable")
	assert.Contains(t, string(calls[1].Body), `"iid":"157"`)
	assert.Equal(t, []string{glTestToken, glTestToken}, gl.tokens)
}

// TestDashboardPRDetails_GitLabWithoutDiffstatFields: an instance whose
// GraphQL lacks the diffstat still gets a card, marked as having none rather
// than claiming an empty change.
func TestDashboardPRDetails_GitLabWithoutDiffstatFields(t *testing.T) {
	mux, _ := glDetailsWorld(t, `{"errors":[{"message":"Field 'diffStatsSummary' doesn't exist on type 'MergeRequest'",
		"extensions":{"code":"undefinedField"}}]}`)
	_ = fetchPRDetails(t, mux, presentMRTestURL)
	agentd.WaitForBackgroundForTest()

	card := fetchPRDetails(t, mux, presentMRTestURL)
	require.NotNil(t, card.Details)
	assert.Equal(t, "Manual CTAD", card.Details.Title)
	assert.True(t, card.Details.NoDiffstat)
	assert.Zero(t, card.Details.Additions)
}

// TestDashboardPRDetails_GitLabFailedDiffstatReadIsAFailedRead: a transient
// GraphQL failure must not cache a card of zeros.
func TestDashboardPRDetails_GitLabFailedDiffstatReadIsAFailedRead(t *testing.T) {
	mux, gl := glDetailsWorld(t, "")
	gl.route = func(req agentd.GitLabRequestForTest) (glCanned, bool) {
		if req.GraphQL {
			return glCanned{Status: http.StatusBadGateway, Body: `{"message":"bad gateway"}`}, true
		}
		return glCanned{Status: http.StatusOK, Body: glDetailsMR}, true
	}
	_ = fetchPRDetails(t, mux, presentMRTestURL)
	agentd.WaitForBackgroundForTest()
	card := fetchPRDetails(t, mux, presentMRTestURL)
	assert.False(t, card.Resolved)
	assert.False(t, card.Refreshing, "the failure cools down")
}

func TestDashboardPRDetails_GitLabSpendsNoTokenOutsideItsGate(t *testing.T) {
	t.Run("an instance that is not configured", func(t *testing.T) {
		mux, gl := glDetailsWorld(t, "{}")
		other := "https://gitlab.other.test/contoso/sub/player/-/merge_requests/157"
		resp := fetchPRDetails(t, mux, other)
		assert.False(t, resp.Refreshing)
		agentd.WaitForBackgroundForTest()
		assert.Zero(t, gl.count(), "nothing is sent to an instance the operator did not configure")
	})
	t.Run("a project off the allow-list", func(t *testing.T) {
		mux, gl := glDetailsWorld(t, "{}")
		elsewhere := strings.Replace(presentMRTestURL, "/contoso/", "/elsewhere/", 1)
		resp := fetchPRDetails(t, mux, elsewhere)
		assert.False(t, resp.Refreshing)
		agentd.WaitForBackgroundForTest()
		assert.Zero(t, gl.count())
	})
	t.Run("the git proxy turned off", func(t *testing.T) {
		mux, gl := glDetailsWorld(t, "{}")
		// gitlab_hosts and the token stay; no allowed_remotes is the proxy off,
		// and with it the credential.
		require.NoError(t, config.Save(&config.Config{Agent: &config.AgentConfig{GitProxy: &config.GitProxyConfig{
			GitLabHosts: map[string]string{"gitlab.contoso.test": glTestBase},
		}}}))
		_ = fetchPRDetails(t, mux, presentMRTestURL)
		agentd.WaitForBackgroundForTest()
		resp := fetchPRDetails(t, mux, presentMRTestURL)
		assert.False(t, resp.Resolved)
		assert.Zero(t, gl.count())
	})
	t.Run("a URL that is neither forge's", func(t *testing.T) {
		mux, _ := glDetailsWorld(t, "{}")
		rec := testharness.Serve(mux, testharness.JSONRequest(t, http.MethodGet,
			"/api/pr-details?url="+url.QueryEscape("https://gitlab.contoso.test:8929/contoso/sub/player/-/issues/3"), nil))
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
}

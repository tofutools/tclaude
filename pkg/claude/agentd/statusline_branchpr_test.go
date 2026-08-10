package agentd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// askBranchPR drives the handler the way the status bar does and decodes the
// answer.
func askBranchPR(t *testing.T, branch string, convID ...string) statuslineBranchPRResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/statusline/branch-pr?branch="+branch, nil)
	if len(convID) > 0 {
		req = AsAgentPeer(req, convID[0])
	}
	handleStatuslineBranchPR(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var out statuslineBranchPRResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&out))
	return out
}

// TestStatuslineBranchPRServesTheAlreadyResolvedPR is the whole point of the
// endpoint: the status bar gets the pull request this daemon has ALREADY
// resolved for the dashboard's Branch column, so the pane spends no GitHub
// credential, needs no grant, and writes no audit row for a link it re-renders
// several times a second.
//
// The first ask is expected to miss — the answer is a cache read, and a cold
// cache has nothing. What makes the endpoint work is that the miss SCHEDULES
// the resolution, so the next ask lands. That two-step is asserted here because
// an implementation that answered from a cold cache without scheduling would
// look identical on a single call and never resolve anything.
func TestStatuslineBranchPRServesTheAlreadyResolvedPR(t *testing.T) {
	setupTestDB(t)
	const convID = "statusline-branchpr-0001"
	const branch = "feature"
	_, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, db.UpsertAgentWorkspace(db.AgentWorkspace{
		ConvID: convID, Cwd: "/repo", Branch: branch,
	}))
	defer SetGitInfoResolverForTest(
		func(string, string) (string, string, int, string, string, bool) {
			return "https://github.com/o/r", "main", 42, "https://github.com/o/r/pull/42", "open", true
		})()

	cold := askBranchPR(t, branch, convID)
	assert.False(t, cold.Resolved,
		"a cold cache reports 'not resolved yet', never 'no pull request' — the caller falls back to gh on this")

	WaitForBackgroundForTest()

	warm := askBranchPR(t, branch, convID)
	require.True(t, warm.Resolved, "the miss scheduled the resolution; the second ask must land")
	assert.Equal(t, 42, warm.PRNumber)
	assert.Equal(t, "https://github.com/o/r/pull/42", warm.PRURL)
	assert.Equal(t, "open", warm.PRState)
}

// TestStatuslineBranchPRResolvesNoPRAsAnAnswer — a freshly pushed branch has no
// pull request, and that is a real, final answer rather than a miss. Reporting
// it as unresolved would send every such pane to `gh` on every render, which is
// exactly the traffic this endpoint exists to remove.
func TestStatuslineBranchPRResolvesNoPRAsAnAnswer(t *testing.T) {
	setupTestDB(t)
	const convID = "statusline-branchpr-0002"
	const branch = "feature"
	_, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, db.UpsertAgentWorkspace(db.AgentWorkspace{
		ConvID: convID, Cwd: "/repo", Branch: branch,
	}))
	defer SetGitInfoResolverForTest(
		func(string, string) (string, string, int, string, string, bool) {
			return "https://github.com/o/r", "main", 0, "", "", true
		})()

	askBranchPR(t, branch, convID)
	WaitForBackgroundForTest()

	warm := askBranchPR(t, branch, convID)
	assert.True(t, warm.Resolved, "'this branch has no PR' is an answer, not a miss")
	assert.Zero(t, warm.PRNumber)
	assert.Empty(t, warm.PRURL)
}

// TestStatuslineBranchPRTakesNoDirectoryFromTheCaller pins the property that
// lets this endpoint carry no permission slug at all.
//
// The repository is resolved from the CALLER'S OWN identity, never from a
// parameter. A directory the caller could name would let any confirmed agent
// ask about any repository on the host — a filesystem reach the proxy
// deliberately refuses to lend, and the one thing that would turn an ungated
// read of the pane's own link into a real widening.
func TestStatuslineBranchPRTakesNoDirectoryFromTheCaller(t *testing.T) {
	setupTestDB(t)
	const convID = "statusline-branchpr-0003"
	_, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, db.UpsertAgentWorkspace(db.AgentWorkspace{
		ConvID: convID, Cwd: "/repo", Branch: "feature",
	}))

	var asked []string
	defer SetGitInfoResolverForTest(
		func(repoDir, _ string) (string, string, int, string, string, bool) {
			asked = append(asked, repoDir)
			return "https://github.com/o/r", "main", 1, "https://github.com/o/r/pull/1", "open", true
		})()

	// A caller trying to name someone else's repository through the query
	// string. The parameter does not exist, so it cannot be honoured.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/v1/statusline/branch-pr?branch=feature&dir=/somebody/else&repo_dir=/somebody/else", nil)
	handleStatuslineBranchPR(rec, AsAgentPeer(req, convID))
	require.Equal(t, http.StatusOK, rec.Code)
	WaitForBackgroundForTest()

	require.NotEmpty(t, asked)
	for _, dir := range asked {
		assert.Equal(t, "/repo", dir,
			"the directory comes from the caller's recorded location, never from the request")
	}
}

// TestStatuslineBranchPRRefusesCallersItCannotPlace — fail closed. An
// unconfirmed caller has no location to resolve, so there is nothing to answer;
// this is the check that keeps that a deliberate refusal rather than an
// accident of the lookup returning empty.
func TestStatuslineBranchPRRefusesCallersItCannotPlace(t *testing.T) {
	setupTestDB(t)
	var resolverCalls int
	defer SetGitInfoResolverForTest(
		func(string, string) (string, string, int, string, string, bool) {
			resolverCalls++
			return "https://github.com/o/r", "main", 1, "https://github.com/o/r/pull/1", "open", true
		})()

	assert.False(t, askBranchPR(t, "feature").Resolved, "no agent identity, no answer")

	// A branch is required: without one there is no cache key, and an empty
	// one must not be read as "every branch".
	const convID = "statusline-branchpr-0004"
	_, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, db.UpsertAgentWorkspace(db.AgentWorkspace{
		ConvID: convID, Cwd: "/repo", Branch: "feature",
	}))
	assert.False(t, askBranchPR(t, "", convID).Resolved)

	WaitForBackgroundForTest()
	assert.Zero(t, resolverCalls, "neither case may reach git or gh")
}

// TestStatuslineBranchPRIsGETOnly — a POST would be recorded by the audit
// middleware, which records mutating methods. This route is a cache read that
// fires on a display refresh, and putting it in the operator's audit trail is
// precisely the cost the endpoint exists to avoid.
func TestStatuslineBranchPRIsGETOnly(t *testing.T) {
	rec := httptest.NewRecorder()
	handleStatuslineBranchPR(rec, httptest.NewRequest(http.MethodPost, "/v1/statusline/branch-pr", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

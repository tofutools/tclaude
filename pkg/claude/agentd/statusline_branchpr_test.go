package agentd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	assert.Empty(t, cold.PRURL,
		"a cold cache has nothing to offer; the caller falls back to gh on this")

	WaitForBackgroundForTest()

	warm := askBranchPR(t, branch, convID)
	assert.Equal(t, 42, warm.PRNumber, "the miss scheduled the resolution; the second ask must land")
	assert.Equal(t, "https://github.com/o/r/pull/42", warm.PRURL)
	assert.Equal(t, "open", warm.PRState)
}

// TestStatuslineBranchPRRefusesABranchThatCouldNameARepository is the property
// that lets this route carry no permission slug and write no audit row.
//
// The branch reaches `gh pr view`'s argv (branchlinks.go, ghPRForBranch), and
// `gh pr view` accepts `<number> | <url> | <branch>`: a URL argument re-aims it
// at ANOTHER REPOSITORY, and a bare number selects a pull request by id. On an
// ungated, unaudited route, a caller-supplied value in that position would let
// any confirmed agent read any pull request the operator's token can reach —
// the first ask schedules `gh pr view https://github.com/victim/private/pull/1`
// with the operator's credential, the second returns it from cache.
//
// The defence is not a sanitiser but the absence of the sink: the caller's
// branch is compared against the daemon's own resolved branch and then
// discarded, so nothing it sends is ever passed on. These cases would each
// survive a plausible charset gate — the scheme-less URL and the bare `1` are
// both legal git ref names — which is why out-guessing another tool's argument
// parser was the wrong shape of fix.
//
// The resolver must never see any of them: a refusal that still reached `gh`
// would spend the credential regardless of what this endpoint returned.
func TestStatuslineBranchPRRefusesABranchThatCouldNameARepository(t *testing.T) {
	setupTestDB(t)
	const convID = "statusline-branchpr-0005"
	_, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, db.UpsertAgentWorkspace(db.AgentWorkspace{
		ConvID: convID, Cwd: "/repo", Branch: "feature",
	}))

	var seen []string
	defer SetGitInfoResolverForTest(
		func(_, branch string) (string, string, int, string, string, bool) {
			seen = append(seen, branch)
			return "https://github.com/o/r", "main", 1, "https://github.com/o/r/pull/1", "open", true
		})()

	for _, branch := range []string{
		"https://github.com/victim/private/pull/1", // a URL re-aims gh at another repo
		"github.com/victim/private/pull/1",
		"--repo=victim/private",
		"-R victim/private",
		"1", // a bare number is a PR selector, not a branch
		"feature branch",
		"feat/../../etc",
		"HEAD",
		"",
		"   ",
	} {
		t.Run(branch, func(t *testing.T) {
			out := askBranchPR(t, url.QueryEscape(branch), convID)
			assert.Empty(t, out.PRURL, "a branch that is not this agent's own must yield nothing")
		})
	}

	WaitForBackgroundForTest()
	assert.Empty(t, seen, "no refused branch may reach git or gh")
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

	assert.Empty(t, askBranchPR(t, "feature").PRURL, "no agent identity, no answer")

	// A branch is required: without one there is no cache key, and an empty
	// one must not be read as "every branch".
	const convID = "statusline-branchpr-0004"
	_, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, db.UpsertAgentWorkspace(db.AgentWorkspace{
		ConvID: convID, Cwd: "/repo", Branch: "feature",
	}))
	assert.Empty(t, askBranchPR(t, "", convID).PRURL)

	WaitForBackgroundForTest()
	assert.Zero(t, resolverCalls, "neither case may reach git or gh")
}

// TestStatuslineBranchPRIsGETOnly. The route is a read and says so; a POST
// would also be the shape the audit middleware inspects, and while its
// allowlist has no entry for this path under any method, a read that fires on
// a display refresh should not be one rename away from the audit trail.
func TestStatuslineBranchPRIsGETOnly(t *testing.T) {
	rec := httptest.NewRecorder()
	handleStatuslineBranchPR(rec, httptest.NewRequest(http.MethodPost, "/v1/statusline/branch-pr", nil))
	assert.Equal(t, http.StatusMethodNotAllowed, rec.Code)
}

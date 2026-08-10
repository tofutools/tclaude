package agentd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
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

// newBranchPRAgent registers a conversation whose LAUNCH DIR is dir — the
// daemon-authored session state this route resolves from. It deliberately does
// not touch agent_workdir or agent_workspace: those are agent-writable and the
// route must not read them.
func newBranchPRAgent(t *testing.T, convID, dir string) {
	t.Helper()
	_, _, err := db.EnsureAgentForConv(convID, "test")
	require.NoError(t, err)
	require.NoError(t, db.SaveSession(&db.SessionRow{
		ID: "sess-" + convID, ConvID: convID, TmuxSession: "tmux-" + convID,
		Cwd: dir, Status: "idle",
	}))
}

// TestStatuslineBranchPRResolvesWithoutADashboard is the whole point of the
// endpoint, and the property most likely to regress silently.
//
// The status bar gets the pull request this daemon resolves for the dashboard's
// Branch column, so the pane spends no GitHub credential, needs no grant, and
// writes no audit row for a link it re-renders several times a second. But
// NOTHING IN agentd RESOLVES BRANCH LINKS ON A TIMER: the only two drivers are
// the dashboard snapshot (`/api/snapshot`, which exists only while a browser is
// polling it) and this route. An endpoint that merely READ the cache would
// therefore answer nothing at all for an operator who never opens the
// dashboard — which is most of them, most of the time.
//
// So the ask has to drive the resolution itself, and this test runs the whole
// sequence without touching a single dashboard code path: cold ask returns
// nothing and schedules the work, the work runs, the next ask lands. The
// assertion that the resolver was invoked by the ask is what distinguishes
// "self-driving" from "happens to find something the dashboard left behind".
func TestStatuslineBranchPRResolvesWithoutADashboard(t *testing.T) {
	setupTestDB(t)
	const convID = "statusline-branchpr-0001"
	const branch = "feature"
	newBranchPRAgent(t, convID, "/repo")
	var resolved int
	defer SetGitInfoResolverForTest(
		func(string, string) (string, string, int, string, string, bool) {
			resolved++
			return "https://github.com/o/r", "main", 42, "https://github.com/o/r/pull/42", "open", true
		})()

	cold := askBranchPR(t, branch, convID)
	assert.Empty(t, cold.PRURL,
		"a cold cache has nothing to offer; the caller falls back to gh on this")

	WaitForBackgroundForTest()
	require.Equal(t, 1, resolved,
		"the ask itself must schedule the resolution — no dashboard poll happened here, and none ever has to")

	warm := askBranchPR(t, branch, convID)
	assert.Equal(t, 42, warm.PRNumber, "the miss scheduled the resolution; the second ask must land")
	assert.Equal(t, "https://github.com/o/r/pull/42", warm.PRURL)
	assert.Equal(t, "open", warm.PRState)

	// And a fresh entry is served without spending another git/gh round: the
	// status bar asks every 15 seconds, the daemon's own TTL is 90, and the
	// single-flight key means several panes on the same branch share one.
	WaitForBackgroundForTest()
	assert.Equal(t, 1, resolved, "a warm entry must not re-resolve on every ask")
}

// TestStatuslineBranchPRRefusesABranchThatCouldNameARepository is the property
// that lets this route carry no permission slug and write no audit row.
//
// The branch reaches `gh`'s argv (branchlinks.go, ghPRForBranch). `gh pr view`
// — which that used to call — accepts `<number> | <url> | <branch>`, so a URL
// argument re-aims it at ANOTHER REPOSITORY and a bare number selects a pull
// request by id: an unaudited, ungated read of any pull request the operator's
// token can reach.
//
// THE BRANCH IS AGENT-CONTROLLED, which is the part that is easy to get wrong.
// It looks daemon-derived — the handler passes `loc.Branch`, not the query
// parameter — but ResolveLocation reads that out of `agent_workspace`, a row
// the agent's own status line writes verbatim through the ungated statusline
// broker. So the equality check against the caller's parameter is no gate at
// all: an attacker supplies both sides. This test therefore writes the hostile
// value the way an agent would, through db.UpsertAgentWorkspace, rather than
// only passing it in the query string.
//
// The defence is at the sink: `gh pr list --head`, which has no
// number-or-URL selector semantics, behind validateBranchName for the shapes
// that would still be read as flags.
func TestStatuslineBranchPRRefusesABranchThatCouldNameARepository(t *testing.T) {
	setupTestDB(t)
	const convID = "statusline-branchpr-0005"
	newBranchPRAgent(t, convID, "/repo")

	var seen []string
	defer SetGitInfoResolverForTest(
		func(_, branch string) (string, string, int, string, string, bool) {
			seen = append(seen, branch)
			return "https://github.com/o/r", "main", 1, "https://github.com/o/r/pull/1", "open", true
		})()

	for _, branch := range []string{
		"https://github.com/victim/private/pull/1", // a URL would re-aim a selector-style gh call
		"--repo=victim/private",                    // reads as a flag in any argv position
		"-R victim/private",
		"feature branch",
		"feat/../../etc",
		"HEAD",
	} {
		t.Run(branch, func(t *testing.T) {
			// The write an agent can perform on itself, unvalidated, through
			// POST /v1/whoami/statusline. This is what makes loc.Branch
			// attacker-chosen.
			require.NoError(t, db.UpsertAgentWorkspace(db.AgentWorkspace{
				ConvID: convID, Cwd: "/repo", Branch: branch,
			}))
			out := askBranchPR(t, url.QueryEscape(branch), convID)
			assert.Empty(t, out.PRURL, "a branch that could name a repository must yield nothing")
		})
	}

	WaitForBackgroundForTest()
	assert.Empty(t, seen, "no such branch may reach git or gh, whichever way it was planted")
}

// TestGHPRListArgsCannotNameARepository is the other half, and the one that
// covers what no charset gate can.
//
// `github.com/victim/private/pull/1` and a bare `1` are LEGAL git ref names, so
// validateBranchName admits them and should: refusing every legal branch that
// resembles a URL would be guesswork. What makes them harmless is the argv
// shape — `pr list --head <branch>` filters by branch, while the `pr view
// <branch>` this replaced would have read them as a repository URL and a pull
// request id. This pins that shape, because reverting it silently restores the
// leak for exactly the inputs the charset gate lets through.
func TestGHPRListArgsCannotNameARepository(t *testing.T) {
	for _, branch := range []string{
		"github.com/victim/private/pull/1",
		"1",
		"feature",
	} {
		t.Run(branch, func(t *testing.T) {
			for _, withChecks := range []bool{true, false} {
				args := ghPRListArgs(branch, withChecks)
				require.Greater(t, len(args), 2)
				assert.Equal(t, []string{"pr", "list"}, args[:2],
					"never `pr view`, whose positional accepts a number or a URL")

				at := slices.Index(args, branch)
				require.Positive(t, at, "the branch must appear in the argv")
				assert.Equal(t, "--head", args[at-1],
					"the branch may only ever be the VALUE of --head, never a bare selector")
			}
		})
	}
}

// TestGHPRForBranchRefusesToLetABranchNameARepository pins the gate at the
// SINK, where the value actually becomes argv — one layer below the endpoint
// above, and shared with the dashboard's own branch-link resolution, which
// reads the same agent-written `agent_workspace` row.
func TestGHPRForBranchRefusesToLetABranchNameARepository(t *testing.T) {
	for _, branch := range []string{
		"https://github.com/victim/private/pull/1",
		"-R victim/private",
		"--repo=victim/private",
		"feature branch",
		"feat/../../etc",
		"HEAD",
		"",
	} {
		t.Run(branch, func(t *testing.T) {
			assert.False(t, safeBranchForGH("/repo", branch),
				"this shape must never be handed to gh")
		})
	}
	for _, branch := range []string{"feature", "feat/thing", "release/1.2"} {
		t.Run("allows "+branch, func(t *testing.T) {
			assert.True(t, safeBranchForGH("/repo", branch),
				"an ordinary branch name must still resolve")
		})
	}
}

// TestStatuslineBranchPRIgnoresTheAgentWritableWorkdir is the property that
// lets this endpoint carry no permission slug, and the one an earlier version
// got wrong.
//
// The tempting source for "which repository is this agent in" is
// agent.ResolveLocation's CurrentDir. It resolves through `agent_workdir`,
// which the PostToolUse hook writes from `filepath.Dir(tool_input.file_path)`
// — a raw payload field, recorded on the failure arm too, through a brokered
// hook route that clamps nothing and carries no permission slug. So an agent
// nominates any path on the host by ATTEMPTING an edit there, and the daemon
// would then run `git` and `gh` with cmd.Dir set to it, where repo-local
// config picks the repository. A branch filter cannot help: cwd chooses the
// repo.
//
// The rest of the codebase already refuses this table as authority in exactly
// these words — resolveProxyRepo "deliberately does NOT consult agent_workdir",
// and spawn_dir_trust.go calls it "display state, not an authorization root".
// This route now resolves the same daemon-authored launch dir the proxy does.
//
// The test plants the hostile row the way the hook would and asserts the
// resolver is never pointed at it. A version that reads CurrentDir fails here.
func TestStatuslineBranchPRIgnoresTheAgentWritableWorkdir(t *testing.T) {
	setupTestDB(t)
	const convID = "statusline-branchpr-0003"
	const launchDir = "/repo"
	const victimDir = "/somebody/elses/private-repo"
	newBranchPRAgent(t, convID, launchDir)

	// The two tables an agent CAN write about itself: the workdir the hook
	// records from a tool payload, and the workspace row its own status line
	// publishes. Neither may decide where the daemon runs gh.
	require.NoError(t, db.UpsertAgentWorkdir(convID, victimDir, victimDir, "release-2026"))
	require.NoError(t, db.UpsertAgentWorkspace(db.AgentWorkspace{
		ConvID: convID, Cwd: victimDir, Branch: "release-2026",
	}))

	var asked []string
	defer SetGitInfoResolverForTest(
		func(repoDir, _ string) (string, string, int, string, string, bool) {
			asked = append(asked, repoDir)
			return "https://github.com/o/r", "main", 1, "https://github.com/o/r/pull/1", "open", true
		})()

	askBranchPR(t, "release-2026", convID)
	WaitForBackgroundForTest()

	require.NotEmpty(t, asked, "the ask must still drive a resolution")
	for _, dir := range asked {
		assert.Equal(t, launchDir, dir,
			"the directory must come from daemon-authored session state, never from agent_workdir")
		assert.NotEqual(t, victimDir, dir)
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
	newBranchPRAgent(t, convID, "/repo")
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

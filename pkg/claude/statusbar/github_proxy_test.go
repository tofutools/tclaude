package statusbar

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
)

// TestPickBranchPRPrefersTheOpenOne — a branch outlives its pull requests: an
// attempt gets closed, another is opened from the same head. The rows arrive
// newest-created first, and the bar must show the LIVE one rather than
// whichever was created last, which is the rule `gh pr view <branch>` follows.
func TestPickBranchPRPrefersTheOpenOne(t *testing.T) {
	prs := []ghProxyPREntry{
		{Number: 9, State: "CLOSED", URL: "u9", HeadRefName: "feat"},
		{Number: 7, State: "OPEN", URL: "u7", HeadRefName: "feat"},
		{Number: 3, State: "MERGED", URL: "u3", HeadRefName: "feat"},
	}
	got := pickBranchPR(prs, "feat")
	require.NotNil(t, got)
	assert.Equal(t, 7, got.Number)
}

// TestPickBranchPRFallsBackToTheNewest — with nothing open, the branch's
// history is still worth a link, and the newest is the one the agent just
// worked on. A merged PR renders with its state; dropping it would make the
// link vanish the moment the work landed.
func TestPickBranchPRFallsBackToTheNewest(t *testing.T) {
	prs := []ghProxyPREntry{
		{Number: 9, State: "MERGED", URL: "u9", HeadRefName: "feat"},
		{Number: 3, State: "CLOSED", URL: "u3", HeadRefName: "feat"},
	}
	got := pickBranchPR(prs, "feat")
	require.NotNil(t, got)
	assert.Equal(t, 9, got.Number)
}

// TestPickBranchPRIgnoresWhatIsNotThisBranchesWork. Showing someone else's PR
// in this branch's bar is worse than showing none: it reads as "my work has a
// PR" when it does not.
//
// The fork case is the one that actually reaches production. GraphQL's
// headRefName filter matches the bare ref NAME across every head repository —
// unlike `gh pr view <branch>`, which compares the head LABEL — so on a public
// repo an outside contributor's `their-fork:fix-typo` comes back in the
// listing for a local branch called `fix-typo`. The other-branch case is the
// cheap guard against the daemon's filter ever widening.
func TestPickBranchPRIgnoresWhatIsNotThisBranchesWork(t *testing.T) {
	fork := []ghProxyPREntry{
		{Number: 9, State: "OPEN", URL: "u9", HeadRefName: "feat", IsCrossRepository: true},
	}
	assert.Nil(t, pickBranchPR(fork, "feat"), "a fork's identically-named branch is not this branch")

	// An open fork PR must not outrank this repository's own, either — the
	// open-beats-newest rule runs after the fork is dropped, not before.
	mixed := []ghProxyPREntry{
		{Number: 9, State: "OPEN", URL: "u9", HeadRefName: "feat", IsCrossRepository: true},
		{Number: 4, State: "MERGED", URL: "u4", HeadRefName: "feat"},
	}
	got := pickBranchPR(mixed, "feat")
	require.NotNil(t, got)
	assert.Equal(t, 4, got.Number)

	other := []ghProxyPREntry{{Number: 9, State: "OPEN", URL: "u9", HeadRefName: "other"}}
	assert.Nil(t, pickBranchPR(other, "feat"))
	assert.Nil(t, pickBranchPR(nil, "feat"))

	// A row that does not say which branch it belongs to does not get the
	// benefit of the doubt. An older daemon that does not know the `head`
	// field answers with every PR in the repository, and crediting an
	// unlabelled row to this branch is how that becomes a wrong link rather
	// than no link.
	unlabelled := []ghProxyPREntry{{Number: 9, State: "OPEN", URL: "u9"}}
	assert.Nil(t, pickBranchPR(unlabelled, "feat"))
}

// TestPRLookupTTLIsSlowerThroughTheProxy pins the whole reason the snapshot
// carries a PRVia at all. A proxied lookup spends the OPERATOR's GitHub
// credential and writes an audit row; a status line re-renders several times a
// second. If these two ever became equal, a handful of panes would turn the
// operator's audit trail into a render log.
func TestPRLookupTTLIsSlowerThroughTheProxy(t *testing.T) {
	assert.Equal(t, gitCacheTTL, prLookupTTL(prViaGH))
	assert.Equal(t, gitCacheTTL, prLookupTTL(""), "an entry from before PRVia existed keeps the old cadence")
	assert.Greater(t, prLookupTTL(prViaProxy), gitCacheTTL)
}

// TestCarryPRForwardKeepsARecentProxyLookup — the carry-forward IS the
// throttle. Without it the proxied path would spend a credentialed call on
// every 15-second snapshot refresh, which is the cost prLookupTTL exists to
// avoid.
func TestCarryPRForwardKeepsARecentProxyLookup(t *testing.T) {
	cached := &GitSnapshot{
		Branch:      "feat",
		PRNumber:    42,
		PRURL:       "https://github.com/o/r/pull/42",
		PRState:     "open",
		PRFetchedAt: time.Now().Add(-30 * time.Second),
		PRVia:       prViaProxy,
	}
	data := &GitSnapshot{Branch: "feat", FetchedAt: time.Now()}

	require.True(t, carryPRForward(cached, data))
	assert.Equal(t, 42, data.PRNumber)
	assert.Equal(t, "open", data.PRState)
	assert.Equal(t, cached.PRFetchedAt, data.PRFetchedAt,
		"carrying a lookup forward must not restamp it as newly observed")
	assert.Equal(t, prViaProxy, data.PRVia)
}

// TestCarryPRForwardKeepsANegativeResult — "this branch has no pull request"
// is the answer for every freshly-pushed feature branch, it costs exactly as
// much to obtain as a positive one, and re-asking it every fifteen seconds is
// the most expensive way to keep learning nothing.
func TestCarryPRForwardKeepsANegativeResult(t *testing.T) {
	cached := &GitSnapshot{
		Branch:      "feat",
		PRFetchedAt: time.Now().Add(-30 * time.Second),
		PRVia:       prViaProxy,
	}
	data := &GitSnapshot{Branch: "feat", FetchedAt: time.Now()}

	assert.True(t, carryPRForward(cached, data))
	assert.Zero(t, data.PRNumber)
}

// TestCarryPRForwardRefusesWhatItCannotVouchFor. Each of these would put a PR
// on the bar that is not this branch's, or hide a change behind a stale one.
func TestCarryPRForwardRefusesWhatItCannotVouchFor(t *testing.T) {
	now := time.Now()
	cases := map[string]*GitSnapshot{
		"no previous snapshot": nil,
		"never looked up": {
			Branch: "feat", PRNumber: 42, PRVia: prViaProxy,
		},
		"a different branch": {
			Branch: "other", PRNumber: 42, PRFetchedAt: now.Add(-time.Second), PRVia: prViaProxy,
		},
		"older than the proxy cadence": {
			Branch: "feat", PRNumber: 42, PRFetchedAt: now.Add(-2 * proxyPRLookupTTL), PRVia: prViaProxy,
		},
		"older than the gh cadence": {
			Branch: "feat", PRNumber: 42, PRFetchedAt: now.Add(-2 * gitCacheTTL), PRVia: prViaGH,
		},
		// A clock that jumped backwards makes the age meaningless, and the
		// safe reading of a meaningless age is "look it up again".
		"stamped in the future": {
			Branch: "feat", PRNumber: 42, PRFetchedAt: now.Add(time.Hour), PRVia: prViaProxy,
		},
	}
	for name, cached := range cases {
		t.Run(name, func(t *testing.T) {
			data := &GitSnapshot{Branch: "feat", FetchedAt: now}
			assert.False(t, carryPRForward(cached, data))
			assert.Zero(t, data.PRNumber)
		})
	}
}

// TestApplyRenderWritesPublishesThePRObservationTime — agentd compares this
// column against its OWN pull-request observation and shows whichever is
// newer. A snapshot whose PR was carried forward from a lookup a minute ago
// must not claim the freshness of the git facts gathered around it, or a stale
// state wins against the daemon's fresher one.
func TestApplyRenderWritesPublishesThePRObservationTime(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	db.ResetForTest()
	t.Cleanup(db.ResetForTest)
	const conv = "statusline-pr-observed-at"
	prFetchedAt := time.Now().Add(-80 * time.Second).Truncate(time.Microsecond)
	input := StatusLineInput{}
	input.Workspace.CurrentDir = "/repo"

	require.True(t, applyRenderWrites(renderWrites{
		Input:         input,
		WorkspaceConv: conv,
		Git: &GitSnapshot{
			RepoURL:       "https://github.com/o/r",
			Branch:        "feature",
			DefaultBranch: "main",
			PRNumber:      42,
			PRURL:         "https://github.com/o/r/pull/42",
			PRState:       "open",
			FetchedAt:     time.Now(),
			PRFetchedAt:   prFetchedAt,
			PRVia:         prViaProxy,
		},
	}))

	ws, err := db.GetAgentWorkspace(conv)
	require.NoError(t, err)
	assert.WithinDuration(t, prFetchedAt, ws.UpdatedAt, time.Microsecond,
		"the published freshness clock must be when the PR was looked up, not when the snapshot was gathered")
}

// TestGitHubProxyEnabledIsFalseWithoutADaemon — the gate fails closed onto the
// behaviour that cannot regress anyone: `gh`, exactly as before. No daemon, an
// older daemon that does not project the capability, a socket that will not
// dial — all of them mean "not through the proxy".
func TestGitHubProxyEnabledIsFalseWithoutADaemon(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	// An absolute override is authoritative in ClientSocketPaths, so this
	// pins the probe to a socket that does not exist rather than letting it
	// find whatever daemon the developer running the tests has going.
	t.Setenv(agentipc.SocketEnv, filepath.Join(dir, "no-such-agentd.sock"))
	assert.False(t, githubProxyEnabled(t.Context()))

	_, _, _, ok := proxyPRInfo(t.Context(), "feat")
	assert.False(t, ok, "an unreachable daemon must not be read as 'this branch has no PR'")
}

// serveFakeProxyDaemon stands a daemon up on the socket path the client
// resolves, so the whole round trip runs rather than being stubbed. Same
// mechanism serveFakeDaemon uses for the render broker, and same reason for
// the short base directory: a unix socket path is capped at ~108 bytes, well
// under what a Go test's temp dir can reach.
func serveFakeProxyDaemon(t *testing.T, handler http.Handler) {
	t.Helper()
	sockDir, err := os.MkdirTemp("/tmp", "tclsb")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "a.sock")
	t.Setenv(agentipc.SocketEnv, sock)

	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	srv := httptest.NewUnstartedServer(handler)
	srv.Listener = ln
	srv.Start()
	t.Cleanup(srv.Close)
}

// infoAnd serves /v1/info with the given github_read bit and routes everything
// else to next.
func infoAnd(githubRead bool, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/info" {
			_ = json.NewEncoder(w).Encode(map[string]any{"github_read": githubRead})
			return
		}
		next(w, r)
	})
}

// TestGitHubProxyEnabledFollowsTheGitHubReadBit — the gate must read the
// narrow bit, not the broad `proxy` one. An agent holding, say, only
// `proxy.git.push` has `proxy` true and `github_read` false, and routing it
// onto the proxy would cost it a refused, audit-logged request every refresh
// for the life of the pane.
func TestGitHubProxyEnabledFollowsTheGitHubReadBit(t *testing.T) {
	t.Run("granted", func(t *testing.T) {
		serveFakeProxyDaemon(t, infoAnd(true, func(http.ResponseWriter, *http.Request) {}))
		assert.True(t, githubProxyEnabled(t.Context()))
	})
	t.Run("not granted", func(t *testing.T) {
		serveFakeProxyDaemon(t, infoAnd(false, func(http.ResponseWriter, *http.Request) {}))
		assert.False(t, githubProxyEnabled(t.Context()))
	})
	t.Run("a daemon too old to project it", func(t *testing.T) {
		serveFakeProxyDaemon(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"proxy": true})
		}))
		assert.False(t, githubProxyEnabled(t.Context()),
			"an absent bit is not a yes — fall back to gh")
	})
}

// TestProxyPRInfoReadsTheBranchesPR — the happy path, and the one place the
// request shape is pinned: the daemon must be asked for THIS branch's pull
// requests in every state, or a just-merged PR's link disappears.
func TestProxyPRInfoReadsTheBranchesPR(t *testing.T) {
	// The handler RECORDS and asserts nothing: httptest runs it on its own
	// goroutine, and require's FailNow is documented as safe only from the
	// goroutine running the test. A failed require here would Goexit the
	// server before it wrote a response, and the test would report an
	// unreachable daemon instead of the mismatch that actually happened.
	var (
		got       map[string]any
		gotPath   string
		decodeErr error
	)
	serveFakeProxyDaemon(t, infoAnd(true, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		decodeErr = json.NewDecoder(r.Body).Decode(&got)
		_ = json.NewEncoder(w).Encode(map[string]any{"exit_code": 0, "json": []map[string]any{
			{"number": 7, "state": "OPEN", "url": "https://github.com/o/r/pull/7", "headRefName": "feat"},
		}})
	}))

	n, u, s, ok := proxyPRInfo(t.Context(), "feat")
	require.NoError(t, decodeErr)
	require.Equal(t, "/v1/github/pr/list", gotPath)
	require.True(t, ok)
	assert.Equal(t, 7, n)
	assert.Equal(t, "https://github.com/o/r/pull/7", u)
	assert.Equal(t, "open", s, "the state is lower-cased, as the gh path's always was")

	assert.Equal(t, "feat", got["head"])
	assert.Equal(t, "all", got["state"])
	assert.Empty(t, got["remote"], "the repository is the daemon's to derive, never the caller's to name")
}

// TestProxyPRInfoSkipsAForkPR — GraphQL's headRefName filter matches the bare
// ref name across every head repository, so a public repo where an outside
// contributor opened a PR from their fork's identically-named branch would
// otherwise render that stranger's PR as the agent's own work.
func TestProxyPRInfoSkipsAForkPR(t *testing.T) {
	serveFakeProxyDaemon(t, infoAnd(true, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"exit_code": 0, "json": []map[string]any{
			{"number": 9, "state": "OPEN", "url": "https://github.com/o/r/pull/9",
				"headRefName": "feat", "isCrossRepository": true},
		}})
	}))

	n, _, _, ok := proxyPRInfo(t.Context(), "feat")
	assert.True(t, ok, "a listing containing only fork PRs is still an answer")
	assert.Zero(t, n)
}

// TestProxyPRInfoSeparatesNoPRFromNoAnswer is the conflation the whole design
// turns on. "No pull request" must be cached; "the lookup failed" must fall
// through to `gh` and be retried — reading one as the other either hides a PR
// that exists or re-spends the operator's credential on a settled answer.
func TestProxyPRInfoSeparatesNoPRFromNoAnswer(t *testing.T) {
	t.Run("empty listing is an answer", func(t *testing.T) {
		serveFakeProxyDaemon(t, infoAnd(true, func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"exit_code": 0, "json": []any{}})
		}))
		n, u, _, ok := proxyPRInfo(t.Context(), "feat")
		assert.True(t, ok)
		assert.Zero(t, n)
		assert.Empty(t, u)
	})
	t.Run("GitHub refused is not an answer", func(t *testing.T) {
		serveFakeProxyDaemon(t, infoAnd(true, func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"exit_code": 1, "stderr": "Resource not accessible by integration"})
		}))
		_, _, _, ok := proxyPRInfo(t.Context(), "feat")
		assert.False(t, ok, "HTTP 200 means the daemon reached GitHub, not that GitHub agreed")
	})
	t.Run("permission denied is not an answer", func(t *testing.T) {
		serveFakeProxyDaemon(t, infoAnd(true, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}))
		_, _, _, ok := proxyPRInfo(t.Context(), "feat")
		assert.False(t, ok)
	})
}

// TestDropForeignRepoPRKeepsTheBarHonest — the bar's repository comes from git
// in the pane's own cwd, while the proxy derives one from the session's
// recorded launch dir. When they diverge the answer describes a repository the
// bar is not rendering.
func TestDropForeignRepoPRKeepsTheBarHonest(t *testing.T) {
	foreign := &GitSnapshot{
		RepoURL: "https://github.com/o/a", Branch: "feat",
		PRNumber: 7, PRURL: "https://github.com/o/b/pull/7", PRState: "open",
		PRVia: prViaProxy, PRFetchedAt: time.Now(),
	}
	dropForeignRepoPR(foreign)
	assert.Zero(t, foreign.PRNumber)
	assert.Empty(t, foreign.PRURL)
	assert.Equal(t, prViaProxy, foreign.PRVia,
		"the lookup still happened; re-asking it every 15s would buy the same wrong answer")

	// A prefix match must not admit a sibling repository whose name merely
	// starts the same way.
	lookalike := &GitSnapshot{
		RepoURL: "https://github.com/o/a", PRURL: "https://github.com/o/ab/pull/7", PRNumber: 7,
	}
	dropForeignRepoPR(lookalike)
	assert.Zero(t, lookalike.PRNumber)

	own := &GitSnapshot{
		RepoURL: "https://github.com/o/a", PRURL: "https://github.com/o/a/pull/7", PRNumber: 7,
	}
	dropForeignRepoPR(own)
	assert.Equal(t, 7, own.PRNumber)

	// The two strings come from different places and GitHub does not force
	// them to agree: the remote is spelled however the operator cloned it,
	// while the PR URL carries the casing GitHub has on record. Same
	// repository — dropping its PR would blank a correct link.
	casing := &GitSnapshot{
		RepoURL: "https://github.com/ToFuTools/A", PRURL: "https://github.com/tofutools/a/pull/7", PRNumber: 7,
	}
	dropForeignRepoPR(casing)
	assert.Equal(t, 7, casing.PRNumber)
}

// TestGetGitDataKeepsAPRWhenNothingCouldLook — the shared budget can be spent
// entirely by a stalled proxy, leaving the `gh` fallback an already-expired
// context so it returns "no pull request" without running. Recording that as
// an observation would suppress the link for a whole proxy interval on one
// transient stall, and would replace a PR the previous snapshot knew about
// with a silence nobody ever checked.
func TestGetGitDataKeepsAPRWhenNothingCouldLook(t *testing.T) {
	known := &GitSnapshot{
		Branch: "feat", PRNumber: 42, PRURL: "https://github.com/o/r/pull/42", PRState: "open",
		PRFetchedAt: time.Now().Add(-2 * proxyPRLookupTTL), PRVia: prViaProxy,
	}
	data := &GitSnapshot{Branch: "feat", FetchedAt: time.Now()}

	require.False(t, carryPRForward(known, data), "the observation is too old to reuse as fresh")
	keepUnrefreshedPR(known, data)

	assert.Equal(t, 42, data.PRNumber, "an unanswered lookup must not blank a known PR")
	assert.Equal(t, known.PRFetchedAt, data.PRFetchedAt,
		"the stale stamp is the point — it is what makes the next refresh try again")
	assert.False(t, carryPRForward(data, &GitSnapshot{Branch: "feat"}),
		"and the carried-over entry is still due for a refresh")

	// A branch flip means the previous snapshot's PR is not this branch's.
	other := &GitSnapshot{Branch: "other", FetchedAt: time.Now()}
	keepUnrefreshedPR(known, other)
	assert.Zero(t, other.PRNumber)
	keepUnrefreshedPR(nil, other)
	assert.Zero(t, other.PRNumber)
}

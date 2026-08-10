package statusbar

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
)

// serveFakeBranchPRDaemon stands a daemon up on the socket path the client
// resolves, so the whole round trip runs rather than being stubbed. Same
// mechanism serveFakeDaemon uses for the render broker, and the same reason for
// the short base directory: a unix socket path is capped at ~108 bytes, well
// under what a Go test's temp dir can reach.
//
// The handler RECORDS and asserts nothing: httptest runs it on its own
// goroutine, where require's FailNow is not safe — it would Goexit the server
// before it wrote a response, and the test would report an unreachable daemon
// instead of the mismatch that actually happened.
func serveFakeBranchPRDaemon(t *testing.T, handler http.HandlerFunc) {
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

// TestDaemonBranchPRReadsTheResolvedPR — the happy path, and the one place the
// request shape is pinned: the branch is sent and the DIRECTORY is not. What
// the daemon does with that — resolving the repository from its own session
// record rather than from any agent-writable table — is the property that lets
// the route be ungated, and it is pinned daemon-side in
// TestStatuslineBranchPRIgnoresTheAgentWritableWorkdir, which is the only place
// it CAN be pinned.
func TestDaemonBranchPRReadsTheResolvedPR(t *testing.T) {
	var gotPath, gotQuery string
	serveFakeBranchPRDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(branchPRResponse{
			PRNumber: 7, PRURL: "https://github.com/o/r/pull/7", PRState: "OPEN",
		})
	})

	n, u, s, ok := daemonBranchPR(t.Context(), "feat/thing")
	require.True(t, ok)
	assert.Equal(t, 7, n)
	assert.Equal(t, "https://github.com/o/r/pull/7", u)
	assert.Equal(t, "open", s, "the state is lower-cased, as the gh path's always was")

	assert.Equal(t, "/v1/statusline/branch-pr", gotPath)
	assert.Equal(t, "branch=feat%2Fthing", gotQuery,
		"the branch is escaped, and it is the only thing sent")
	assert.NotContains(t, gotQuery, "dir")
}

// TestDaemonBranchPRTreatsAnEmptyAnswerAsNoAnswer — a pull request is the only
// success. The daemon is deliberately not asked to distinguish "I looked and
// there is none" from "I have not looked yet": it stamps its cache on every
// outcome, including a `gh` that failed and a directory that resolved to the
// wrong repository, so a flag saying otherwise would suppress the `gh` that
// would have found the PR.
func TestDaemonBranchPRTreatsAnEmptyAnswerAsNoAnswer(t *testing.T) {
	t.Run("no PR in the answer", func(t *testing.T) {
		serveFakeBranchPRDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(branchPRResponse{})
		})
		_, _, _, ok := daemonBranchPR(t.Context(), "feat")
		assert.False(t, ok, "an empty answer must fall through to gh, exactly as before this route existed")
	})
	t.Run("a refusal", func(t *testing.T) {
		serveFakeBranchPRDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		})
		_, _, _, ok := daemonBranchPR(t.Context(), "feat")
		assert.False(t, ok)
	})
	t.Run("junk body", func(t *testing.T) {
		serveFakeBranchPRDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte("not json"))
		})
		_, _, _, ok := daemonBranchPR(t.Context(), "feat")
		assert.False(t, ok)
	})
}

// TestDaemonBranchPRWithoutADaemon — no daemon means `gh`, exactly as before.
// This is the path every unmanaged pane takes, and it must not be read as
// "this branch has no pull request".
func TestDaemonBranchPRWithoutADaemon(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	// An absolute override is authoritative in ClientSocketPaths, so this pins
	// the client to a socket that does not exist rather than letting it find
	// whatever daemon the developer running the tests has going.
	t.Setenv(agentipc.SocketEnv, filepath.Join(dir, "no-such-agentd.sock"))

	_, _, _, ok := daemonBranchPR(t.Context(), "feat")
	assert.False(t, ok)
}

// TestDropForeignRepoPRKeepsTheBarHonest — the bar's repository comes from git
// in the pane's own cwd, while the daemon answers for the directory it has
// recorded as this agent's. When they diverge, the answer describes a
// repository the bar is not rendering.
func TestDropForeignRepoPRKeepsTheBarHonest(t *testing.T) {
	foreign := &GitSnapshot{
		RepoURL: "https://github.com/o/a", Branch: "feat",
		PRNumber: 7, PRURL: "https://github.com/o/b/pull/7", PRState: "open",
	}
	dropForeignRepoPR(foreign)
	assert.Zero(t, foreign.PRNumber)
	assert.Empty(t, foreign.PRURL)

	// A prefix match must not admit a sibling repository whose name merely
	// starts the same way.
	lookalike := &GitSnapshot{
		RepoURL: "https://github.com/o/a", PRURL: "https://github.com/o/ab/pull/7", PRNumber: 7,
	}
	dropForeignRepoPR(lookalike)
	assert.Zero(t, lookalike.PRNumber)

	// Every spelling of the SAME repository must be kept. The remote is
	// written however the operator cloned it, while the PR URL carries what
	// GitHub has on record — a prefix test would call most of these foreign
	// and silently blank a correct link, on the `gh` path too.
	for _, repoURL := range []string{
		"https://github.com/o/a",
		"https://github.com/o/a/",
		"ssh://git@github.com/o/a",
		"ssh://git@github.com/o/a.git",
		"http://github.com/o/a",
		"ssh://git@github.com:22/o/r.git", // an explicit port is not a path segment
		"https://github.com/ToFuTools/A",  /* GitHub's casing differs from the clone's */
	} {
		t.Run(repoURL, func(t *testing.T) {
			own := &GitSnapshot{RepoURL: repoURL, PRNumber: 7,
				PRURL: "https://github.com/tofutools/a/pull/7"}
			switch {
			case strings.Contains(repoURL, "/o/r"):
				own.PRURL = "https://github.com/o/r/pull/7"
			case !strings.Contains(strings.ToLower(repoURL), "tofutools"):
				own.PRURL = "https://github.com/o/a/pull/7"
			}
			dropForeignRepoPR(own)
			assert.Equal(t, 7, own.PRNumber, "%s is the same repository as its own PR", repoURL)
		})
	}

	// A repo URL this cannot parse is not evidence of anything, and blanking
	// on it would be the same silent-drop bug in the other direction.
	unparseable := &GitSnapshot{
		RepoURL: "github.com", PRURL: "https://github.com/o/a/pull/7", PRNumber: 7,
	}
	dropForeignRepoPR(unparseable)
	assert.Equal(t, 7, unparseable.PRNumber)

	// The scp-like form is what every non-github.com host arrives as —
	// getRepoHTTPS rewrites only `git@github.com:` — so failing to parse it
	// would switch the guard off entirely for those panes, which is when it
	// most needs to be on: the daemon's recorded dir may be a DIFFERENT repo
	// on a same-named branch.
	scpForeign := &GitSnapshot{
		RepoURL: "git@github.corp.com:o/a", PRURL: "https://github.com/other/repo/pull/7", PRNumber: 7,
	}
	dropForeignRepoPR(scpForeign)
	assert.Zero(t, scpForeign.PRNumber, "a scp-like remote must still be compared, not waved through")

	scpOwn := &GitSnapshot{
		RepoURL: "git@gitlab.com:o/a", PRURL: "https://gitlab.com/o/a/pull/7", PRNumber: 7,
	}
	dropForeignRepoPR(scpOwn)
	assert.Equal(t, 7, scpOwn.PRNumber, "and its own PR must survive")
}

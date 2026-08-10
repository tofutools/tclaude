package statusbar

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
// request shape is pinned. The branch is sent; the DIRECTORY is not, and that
// is the property that lets this endpoint be ungated: the daemon resolves the
// repository from the caller's own identity, so no pane can point it at a
// repository that is not its own.
func TestDaemonBranchPRReadsTheResolvedPR(t *testing.T) {
	var gotPath, gotQuery string
	serveFakeBranchPRDaemon(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(branchPRResponse{
			Resolved: true, PRNumber: 7,
			PRURL: "https://github.com/o/r/pull/7", PRState: "OPEN",
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

// TestDaemonBranchPRSeparatesNoPRFromNotResolvedYet is the distinction the
// fallback turns on. "Resolved, no pull request" is a real answer and the usual
// one on a freshly pushed branch. "Not resolved yet" is what a cold cache says
// on the first ask — and rendering that as "no PR" would blank the link of
// every agent whose daemon has not warmed up, when `gh` could have answered.
func TestDaemonBranchPRSeparatesNoPRFromNotResolvedYet(t *testing.T) {
	t.Run("resolved with no PR is an answer", func(t *testing.T) {
		serveFakeBranchPRDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(branchPRResponse{Resolved: true})
		})
		n, u, _, ok := daemonBranchPR(t.Context(), "feat")
		assert.True(t, ok, "the daemon looked and there is no PR — do not go on to ask gh")
		assert.Zero(t, n)
		assert.Empty(t, u)
	})
	t.Run("not resolved yet is not an answer", func(t *testing.T) {
		serveFakeBranchPRDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(branchPRResponse{Resolved: false})
		})
		_, _, _, ok := daemonBranchPR(t.Context(), "feat")
		assert.False(t, ok, "a cold cache must fall through to gh, not render as 'no PR'")
	})
	t.Run("a refusal is not an answer", func(t *testing.T) {
		serveFakeBranchPRDaemon(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
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

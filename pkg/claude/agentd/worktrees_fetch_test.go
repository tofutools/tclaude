package agentd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/worktree"
)

func TestDashboardCreateWorktree_FetchLatestUsesFreshConfiguredUpstream(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	bin := filepath.Join(home, "bin")
	require.NoError(t, os.MkdirAll(bin, 0o755))
	// Keep the test offline while exercising the production SSH-only transport
	// pins: Git asks this shim to run upload-pack for the absolute bare path.
	sshShim := "#!/bin/sh\nfor arg do command=$arg; done\nexec /bin/sh -c \"$command\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, "ssh"), []byte(sshShim), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	repo, writer, initial, latest := worktreeFetchFixture(t)
	hookSentinel := filepath.Join(home, "repo-hook-ran")
	hook := "#!/bin/sh\ntouch '" + hookSentinel + "'\n"
	require.NoError(t, os.WriteFile(filepath.Join(repo, ".git", "hooks", "reference-transaction"), []byte(hook), 0o755))

	off := postDashboardWorktree(t, map[string]any{
		"repo": repo, "branch": "stale-base", "from_branch": "main", "fetch_latest": false,
	})
	require.Equalf(t, http.StatusOK, off.Code, "body=%s", off.Body.String())
	assert.Equal(t, initial, gitOutput(t, filepath.Join(filepath.Dir(repo), "local-stale-base"), "rev-parse", "HEAD"),
		"opting out preserves today's local-base behavior")

	on := postDashboardWorktree(t, map[string]any{
		"repo": repo, "branch": "fresh-base", "from_branch": "main", "fetch_latest": true,
	})
	require.Equalf(t, http.StatusOK, on.Code, "body=%s", on.Body.String())
	assert.Equal(t, latest, gitOutput(t, filepath.Join(filepath.Dir(repo), "local-fresh-base"), "rev-parse", "HEAD"),
		"the new branch is cut from the fetched remote-tracking ref, not stale local main")
	assert.Equal(t, latest, gitOutput(t, repo, "rev-parse", "refs/remotes/upstream/main"),
		"the configured non-origin upstream is refreshed")
	_, hookErr := os.Stat(hookSentinel)
	assert.Truef(t, os.IsNotExist(hookErr), "repo-controlled fetch hooks must not run in the daemon; stat=%v", hookErr)

	// A requested fresh base is fail-closed: losing the remote leaves neither
	// a branch nor a worktree behind.
	gitRun(t, repo, "remote", "set-url", "upstream", "ssh://example.invalid"+filepath.Join(filepath.Dir(writer), "missing.git"))
	failed := postDashboardWorktree(t, map[string]any{
		"repo": repo, "branch": "fetch-failed", "from_branch": "main", "fetch_latest": true,
	})
	assert.Equal(t, http.StatusBadGateway, failed.Code)
	assert.Contains(t, failed.Body.String(), "fetch latest worktree base")
	assert.False(t, worktree.BranchExistsIn(repo, "fetch-failed"))
}

func postDashboardWorktree(t *testing.T, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/worktrees", bytes.NewReader(b))
	rec := httptest.NewRecorder()
	dashboardCreateWorktree(rec, req)
	return rec
}

func worktreeFetchFixture(t *testing.T) (local, writer, initial, latest string) {
	t.Helper()
	root := t.TempDir()
	seed := filepath.Join(root, "seed")
	remote := filepath.Join(root, "remote.git")
	local = filepath.Join(root, "local")
	writer = filepath.Join(root, "writer")
	require.NoError(t, os.Mkdir(seed, 0o755))
	gitRun(t, seed, "init", "-q")
	gitRun(t, seed, "config", "user.email", "test@example.invalid")
	gitRun(t, seed, "config", "user.name", "test")
	require.NoError(t, os.WriteFile(filepath.Join(seed, "version.txt"), []byte("initial\n"), 0o644))
	gitRun(t, seed, "add", "version.txt")
	gitRun(t, seed, "commit", "-qm", "initial")
	gitRun(t, seed, "branch", "-M", "main")
	initial = gitOutput(t, seed, "rev-parse", "HEAD")
	gitRun(t, root, "init", "-q", "--bare", remote)
	gitRun(t, seed, "remote", "add", "target", remote)
	gitRun(t, seed, "push", "-q", "target", "main")
	gitRun(t, remote, "symbolic-ref", "HEAD", "refs/heads/main")
	gitRun(t, root, "clone", "-q", "--origin", "upstream", remote, local)
	gitRun(t, local, "remote", "set-url", "upstream", "ssh://example.invalid"+remote)
	gitRun(t, root, "clone", "-q", remote, writer)
	gitRun(t, writer, "config", "user.email", "test@example.invalid")
	gitRun(t, writer, "config", "user.name", "test")
	require.NoError(t, os.WriteFile(filepath.Join(writer, "version.txt"), []byte("latest\n"), 0o644))
	gitRun(t, writer, "add", "version.txt")
	gitRun(t, writer, "commit", "-qm", "latest")
	latest = gitOutput(t, writer, "rev-parse", "HEAD")
	gitRun(t, writer, "push", "-q", "origin", "main")
	return local, writer, initial, latest
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %s: %s", strings.Join(args, " "), out)
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoErrorf(t, err, "git %s: %s", strings.Join(args, " "), out)
	return strings.TrimSpace(string(out))
}

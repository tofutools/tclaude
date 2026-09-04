package agentd_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/agentd"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/testharness"
)

func repoGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}
func repoCommit(t *testing.T, path, name string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(path, name), []byte(name), 0600))
	repoGit(t, path, "add", "--", name)
	repoGit(t, path, "-c", "commit.gpgsign=false", "commit", "-m", name)
}
func gitFixture(t *testing.T) (string, string, string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	source := filepath.Join(base, "source")
	checkout := filepath.Join(base, "checkout")
	repoGit(t, base, "init", "--bare", "--initial-branch=trunk", remote)
	repoGit(t, base, "clone", remote, source)
	repoCommit(t, source, "initial")
	repoGit(t, source, "push", "-u", "origin", "trunk")
	repoGit(t, base, "clone", remote, checkout)
	f := newFlow(t)
	f.HaveGroup("code")
	_, err := db.SetAgentGroupDefaultCwd("code", checkout)
	require.NoError(t, err)
	return remote, source, checkout
}
func gitAPI(t *testing.T, method string, body map[string]any, target any) int {
	t.Helper()
	var b bytes.Buffer
	if body != nil {
		require.NoError(t, json.NewEncoder(&b).Encode(body))
	}
	req, err := http.NewRequest(method, "/api/git-repositories", &b)
	require.NoError(t, err)
	rec := testharness.Serve(agentd.BuildDashboardHandlerForTest(), req)
	if target != nil {
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), target))
	}
	return rec.Code
}
func updateGit(t *testing.T, path string, switchDefault, discard bool, mode string) map[string]string {
	t.Helper()
	var out map[string]string
	gitAPI(t, http.MethodPost, map[string]any{"group": "code", "path": path, "mode": mode, "switch_default": switchDefault, "discard": discard}, &out)
	return out
}

func TestDashboardGitDiscoveryAndPull(t *testing.T) {
	_, source, checkout := gitFixture(t)
	_, err := db.CreateAgentGroup("alias", "")
	require.NoError(t, err)
	alias := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(checkout, alias))
	_, err = db.SetAgentGroupDefaultCwd("alias", alias)
	require.NoError(t, err)
	var out struct {
		Repos []struct {
			Path, Branch string
			Default      string `json:"default_branch"`
			Groups       []string
		}
	}
	gitAPI(t, http.MethodGet, nil, &out)
	require.Len(t, out.Repos, 1)
	require.Equal(t, "trunk", out.Repos[0].Default)
	require.ElementsMatch(t, []string{"code", "alias"}, out.Repos[0].Groups)
	repoGit(t, checkout, "switch", "-c", "feature")
	repoCommit(t, source, "new-file")
	repoGit(t, source, "push")
	result := updateGit(t, checkout, true, false, "pull")
	require.Equal(t, "updated", result["status"], result)
	require.Equal(t, "trunk", repoGit(t, checkout, "branch", "--show-current"))
	require.FileExists(t, filepath.Join(checkout, "new-file"))
	require.Equal(t, http.StatusConflict, gitAPI(t, http.MethodPost, map[string]any{"path": source, "mode": "pull"}, nil))
}

func TestDashboardGitDirtyDiscardAndPrune(t *testing.T) {
	_, source, checkout := gitFixture(t)
	repoGit(t, source, "push", "origin", "trunk:obsolete")
	repoGit(t, checkout, "fetch")
	repoGit(t, source, "push", "origin", "--delete", "obsolete")
	require.NoError(t, os.WriteFile(filepath.Join(checkout, "initial"), []byte("edited"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(checkout, "untracked"), []byte("keep"), 0600))
	require.Equal(t, "skipped", updateGit(t, checkout, true, false, "pull")["status"])
	require.FileExists(t, filepath.Join(checkout, "untracked"))
	repoCommit(t, source, "latest")
	repoGit(t, source, "push")
	result := updateGit(t, checkout, true, true, "sync")
	require.Equal(t, "updated", result["status"], result)
	require.NoFileExists(t, filepath.Join(checkout, "untracked"))
	require.FileExists(t, filepath.Join(checkout, "latest"))
	require.NotContains(t, repoGit(t, checkout, "branch", "-r"), "obsolete")
}

func TestDashboardGitDivergenceDoesNotDiscard(t *testing.T) {
	_, source, checkout := gitFixture(t)
	repoCommit(t, checkout, "local-commit")
	repoCommit(t, source, "remote-commit")
	repoGit(t, source, "push")
	require.NoError(t, os.WriteFile(filepath.Join(checkout, "precious"), []byte("keep"), 0600))
	result := updateGit(t, checkout, true, true, "pull")
	require.Equal(t, "skipped", result["status"], result)
	require.Contains(t, result["detail"], "diverged")
	require.FileExists(t, filepath.Join(checkout, "precious"))
}

func TestDashboardGitCurrentBranchUpstream(t *testing.T) {
	_, source, checkout := gitFixture(t)
	repoGit(t, source, "switch", "-c", "topic")
	repoCommit(t, source, "topic-file")
	repoGit(t, source, "push", "-u", "origin", "topic")
	repoGit(t, checkout, "fetch")
	repoGit(t, checkout, "switch", "-c", "local-name", "--track", "origin/topic")
	repoCommit(t, source, "topic-new")
	repoGit(t, source, "push")
	result := updateGit(t, checkout, false, false, "pull")
	require.Equal(t, "updated", result["status"], result)
	require.Equal(t, "local-name", repoGit(t, checkout, "branch", "--show-current"))
	require.FileExists(t, filepath.Join(checkout, "topic-new"))
}

func TestDashboardGitWorktreeConflictKeepsChanges(t *testing.T) {
	_, _, checkout := gitFixture(t)
	linked := filepath.Join(t.TempDir(), "linked")
	repoGit(t, checkout, "worktree", "add", "-b", "feature", linked)
	_, err := db.SetAgentGroupDefaultCwd("code", linked)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(linked, "precious"), []byte("keep"), 0600))
	result := updateGit(t, linked, true, true, "pull")
	require.Equal(t, "skipped", result["status"], result)
	require.Contains(t, result["detail"], "another worktree")
	require.FileExists(t, filepath.Join(linked, "precious"))
}

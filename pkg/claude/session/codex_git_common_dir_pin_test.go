package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestCodexGitCommonDirPinValidationAllowsResume(t *testing.T) {
	p := &NewParams{
		Resume:                  "conv",
		DirWriteProof:           "proof_123",
		CodexGitCommonDir:       "/tmp/repo/.git",
		CodexGitCommonDirPinned: true,
	}
	require.NoError(t, validateCodexGitCommonDirPin(p))
}

func TestCodexGitCommonDirPinValidationRejectsPathWithoutPresence(t *testing.T) {
	p := &NewParams{CodexGitCommonDir: "/tmp/repo/.git"}
	require.ErrorContains(t, validateCodexGitCommonDirPin(p), "requires a pinned result")
}

func TestCodexGitCommonDirPinValidationRejectsPathWithoutProof(t *testing.T) {
	p := &NewParams{CodexGitCommonDir: "/tmp/repo/.git", CodexGitCommonDirPinned: true}
	require.ErrorContains(t, validateCodexGitCommonDirPin(p), "requires a daemon write proof")
}

func TestGitWorktreeWriteDirPinsRequireDaemonProof(t *testing.T) {
	p := &NewParams{GitWorktreeWriteDirs: []string{"/tmp/repo-parent"}, GitWorktreeWriteDirsPinned: true}
	require.ErrorContains(t, validateGitWorktreeWriteDirPins(p), "require a daemon write proof")
	p.DirWriteProof = "proof_123"
	require.NoError(t, validateGitWorktreeWriteDirPins(p))
}

func TestEnsureCodexManagedProfilePinnedEmptyDoesNotDeriveFromCwd(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	repo := filepath.Join(home, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))
	cmd := exec.Command("git", "init", "-q", repo)
	require.NoError(t, cmd.Run())
	commonDir, err := harness.CodexGitCommonDir(repo)
	require.NoError(t, err)
	require.NotEmpty(t, commonDir)

	pinnedEmpty := &NewParams{
		PermissionProfile:       harness.CodexAgentProfile,
		CodexGitCommonDirPinned: true,
	}
	profileName, profilePath, err := ensureCodexManagedProfile(pinnedEmpty, repo, "1111111111111111")
	require.NoError(t, err)
	assert.Equal(t, harness.CodexAgentProfile+"-1111111111111111", profileName)
	raw, err := os.ReadFile(profilePath)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), commonDir,
		"pinned-empty must write the base profile instead of deriving from cwd")

	unpinned := &NewParams{PermissionProfile: harness.CodexAgentProfile}
	_, profilePath, err = ensureCodexManagedProfile(unpinned, repo, "2222222222222222")
	require.NoError(t, err)
	raw, err = os.ReadFile(profilePath)
	require.NoError(t, err)
	resolvedRepo, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(raw), resolvedRepo),
		"an intentionally unpinned direct launch still derives from cwd")
}

func TestCommandWithFileCleanupPreservesExitAndRemovesProfile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "launch-profile.toml")
	require.NoError(t, os.WriteFile(path, []byte("profile"), 0o600))
	err := exec.Command("sh", "-c", commandWithFileCleanup("sh -c 'exit 7'", path)).Run()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	assert.Equal(t, 7, exitErr.ExitCode())
	assert.NoFileExists(t, path)
}

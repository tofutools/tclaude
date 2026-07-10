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
		CodexGitCommonDir:       "/tmp/repo/.git",
		CodexGitCommonDirPinned: true,
	}
	require.NoError(t, validateCodexGitCommonDirPin(p))
}

func TestCodexGitCommonDirPinValidationRejectsPathWithoutPresence(t *testing.T) {
	p := &NewParams{CodexGitCommonDir: "/tmp/repo/.git"}
	require.ErrorContains(t, validateCodexGitCommonDirPin(p), "requires a pinned result")
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
	require.NoError(t, ensureCodexManagedProfile(pinnedEmpty, repo))
	profilePath, err := harness.CodexAgentProfilePath()
	require.NoError(t, err)
	raw, err := os.ReadFile(profilePath)
	require.NoError(t, err)
	assert.NotContains(t, string(raw), commonDir,
		"pinned-empty must write the base profile instead of deriving from cwd")

	unpinned := &NewParams{PermissionProfile: harness.CodexAgentProfile}
	require.NoError(t, ensureCodexManagedProfile(unpinned, repo))
	raw, err = os.ReadFile(profilePath)
	require.NoError(t, err)
	assert.True(t, strings.Contains(string(raw), commonDir),
		"an intentionally unpinned direct launch still derives from cwd")
}

package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestCleanupCodexLaunchProfile_FinalReconcileAndRemove(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	_, profilePath, err := harness.EnsureCodexAgentLaunchProfile(nil, "9999999999999999")
	require.NoError(t, err)
	f, err := os.OpenFile(profilePath, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString("\n[apps.asdk_app_69a089a326dc8191b32a3f2553f5be2c.tools.\"linear.save_issue\"]\n" +
		"approval_mode = \"approve\"\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	require.NoError(t, cleanupCodexLaunchProfile(profilePath))
	assert.NoFileExists(t, profilePath)
	config, err := os.ReadFile(filepath.Join(home, ".codex", "config.toml"))
	require.NoError(t, err)
	assert.Contains(t, string(config), "linear.save_issue")
	assert.Contains(t, string(config), `approval_mode = "approve"`)
}

func TestCodexProfileCleanupShell_HasLegacyRemoveFallback(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // a hostile project-local PATH entry is ignored
	got := CodexProfileCleanupShell("/tmp/profile with spaces.config.toml")
	cleanup := clcommon.DetectAbsoluteCmd("session", "codex-profile-cleanup")
	assert.Contains(t, got, cleanup+" --path")
	assert.False(t, strings.HasPrefix(got, "tclaude "))
	assert.Contains(t, got, cleanup+" --help", "fallback is used only when the installed binary lacks the command")
	assert.Contains(t, got, "|| rm -f --")
}

func TestCleanupCodexLaunchProfile_RefusesUnmanagedPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	require.NoError(t, os.MkdirAll(filepath.Join(home, ".codex"), 0o700))

	unrelated := filepath.Join(home, "keep-me")
	require.NoError(t, os.WriteFile(unrelated, []byte("important"), 0o600))
	require.ErrorContains(t, cleanupCodexLaunchProfile(unrelated), "non-managed")
	assert.FileExists(t, unrelated)

	dir := filepath.Join(home, ".codex", "tclaude-agent-aaaaaaaaaaaaaaaa.config.toml")
	require.NoError(t, os.Mkdir(dir, 0o700))
	require.ErrorContains(t, cleanupCodexLaunchProfile(dir), "not a regular file")
	assert.DirExists(t, dir)
}

func TestCleanupCodexLaunchProfile_RetainsProfileAfterTransientMergeFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	_, profilePath, err := harness.EnsureCodexAgentLaunchProfile(nil, "bbbbbbbbbbbbbbbb")
	require.NoError(t, err)
	f, err := os.OpenFile(profilePath, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString("\n[apps.asdk_app_69a089a326dc8191b32a3f2553f5be2c.tools.\"linear.save_issue\"]\n" +
		"approval_mode = \"approve\"\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	configPath := filepath.Join(home, ".codex", "config.toml")
	require.NoError(t, os.WriteFile(configPath, []byte("invalid = [\n"), 0o600))

	err = cleanupCodexLaunchProfile(profilePath)
	require.ErrorContains(t, err, "profile retained")
	assert.FileExists(t, profilePath)

	require.NoError(t, os.WriteFile(configPath, []byte("# repaired\n"), 0o600))
	require.NoError(t, cleanupCodexLaunchProfile(profilePath))
	assert.NoFileExists(t, profilePath)
}

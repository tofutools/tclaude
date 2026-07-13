package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	got := CodexProfileCleanupShell("/tmp/profile with spaces.config.toml")
	assert.Contains(t, got, "tclaude session codex-profile-cleanup --path")
	assert.Contains(t, got, "|| rm -f --")
}

package harnesscredentials

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func homeFixture(t *testing.T) string {
	t.Helper()
	t.Setenv("CODEX_HOME", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("XDG_DATA_HOME", "")
	return testutil.CanonicalTempDir(t)
}
func TestCredentialCopyConfirmationBackupAndPermissions(t *testing.T) {
	source := homeFixture(t)
	target := testutil.CanonicalTempDir(t)
	require.NoError(t, os.Mkdir(filepath.Join(source, ".codex"), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(source, ".codex", "auth.json"), []byte(`{"token":"source"}`), 0600))
	b, err := Capture(source, "codex")
	require.NoError(t, err)
	require.NoError(t, os.Mkdir(filepath.Join(target, ".codex"), 0700))
	path := filepath.Join(target, ".codex", "auth.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"token":"previous"}`), 0600))
	private := filepath.Join(target, "private")
	_, err = Receive(target, private, b, false, func() bool { return true })
	require.ErrorIs(t, err, ErrExists)
	receipt, err := Receive(target, private, b, true, func() bool { return true })
	require.NoError(t, err)
	require.True(t, receipt.Copied)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, `{"token":"source"}`, string(raw))
	raw, err = os.ReadFile(receipt.BackupLocation)
	require.NoError(t, err)
	require.NotEmpty(t, raw)
	for _, p := range []string{path, receipt.BackupLocation} {
		info, err := os.Stat(p)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	}
}
func TestCredentialsRefuseLinksAndRevokedAuthority(t *testing.T) {
	home := homeFixture(t)
	outside := testutil.CanonicalTempDir(t)
	require.NoError(t, os.Mkdir(filepath.Join(home, ".codex"), 0700))
	secret := filepath.Join(outside, "other")
	require.NoError(t, os.WriteFile(secret, []byte(`{"secret":"other"}`), 0600))
	path := filepath.Join(home, ".codex", "auth.json")
	require.NoError(t, os.Symlink(secret, path))
	_, err := Capture(home, "codex")
	require.Error(t, err)
	b := Bundle{Harness: "codex", Files: []File{{"auth.json", []byte(`{"token":"sender"}`)}}}
	_, err = Receive(home, filepath.Join(home, "private"), b, true, func() bool { return true })
	require.Error(t, err)
	require.NoError(t, os.Remove(path))
	_, err = Receive(home, filepath.Join(home, "private"), b, false, func() bool { return false })
	require.Error(t, err)
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err))
	require.NoError(t, os.Remove(filepath.Join(home, ".codex")))
	require.NoError(t, os.Symlink(outside, filepath.Join(home, ".codex")))
	_, err = Receive(home, filepath.Join(home, "private"), b, false, func() bool { return true })
	require.Error(t, err)
	raw, err := os.ReadFile(secret)
	require.NoError(t, err)
	require.Equal(t, `{"secret":"other"}`, string(raw))
}
func TestCredentialBundleRejectsForeignPathsAndOversize(t *testing.T) {
	for _, b := range []Bundle{{Harness: "codex", Files: []File{{"../auth.json", []byte(`{}`)}}}, {Harness: "codex", Files: []File{{"config.toml", []byte(`{}`)}}}, {Harness: "codex", Files: []File{{"auth.json", []byte(`{}`)}, {"auth.json", []byte(`{}`)}}}, {Harness: "copilot"}, {Harness: "codex", Files: []File{{"auth.json", []byte(`not json`)}}}} {
		require.Error(t, b.Validate())
	}
}

func TestGeminiConfiguredHomeAndClaudeReceivingRoot(t *testing.T) {
	home := homeFixture(t)
	configured := testutil.CanonicalTempDir(t)
	t.Setenv("GEMINI_CLI_HOME", configured)
	dir := filepath.Join(configured, ".gemini")
	require.NoError(t, os.Mkdir(dir, 0700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "oauth_creds.json"), []byte(`{"token":"source"}`), 0600))
	b, err := Capture(home, "gemini")
	require.NoError(t, err)
	target := testutil.CanonicalTempDir(t)
	t.Setenv("GEMINI_CLI_HOME", target)
	receipt, err := Receive(home, filepath.Join(home, "private"), b, false, func() bool { return true })
	require.NoError(t, err)
	require.True(t, receipt.Copied)
	raw, err := os.ReadFile(filepath.Join(target, ".gemini", "oauth_creds.json"))
	require.NoError(t, err)
	require.Equal(t, `{"token":"source"}`, string(raw))
	_, err = os.Stat(filepath.Join(home, ".gemini"))
	require.True(t, os.IsNotExist(err))
	t.Setenv("CLAUDE_CONFIG_DIR", configured)
	b = Bundle{Harness: "claude", Files: []File{{".credentials.json", []byte(`{"token":"claude-source"}`)}}}
	_, err = Receive(home, filepath.Join(home, "private"), b, false, func() bool { return true })
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(home, ".claude", ".credentials.json"))
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(configured, ".credentials.json"))
	require.True(t, os.IsNotExist(err))
}

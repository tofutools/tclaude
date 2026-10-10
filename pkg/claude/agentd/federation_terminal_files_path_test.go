package agentd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/testutil"
)

func TestTerminalFilePathSafety(t *testing.T) {
	rootPath := testutil.CanonicalTempDir(t)
	root, _, err := openTerminalFileRoot(rootPath)
	require.NoError(t, err)
	defer root.Close()
	require.NoError(t, os.WriteFile(filepath.Join(rootPath, "ok.txt"), []byte("hello"), 0600))
	require.NoError(t, os.Symlink("ok.txt", filepath.Join(rootPath, "alias")))
	require.NoError(t, os.Mkdir(filepath.Join(rootPath, "nested"), 0700))
	require.NoError(t, os.Symlink("nested", filepath.Join(rootPath, "diralias")))
	require.NoError(t, os.WriteFile(filepath.Join(rootPath, "nested", "good"), nil, 0600))
	for _, name := range []string{"ok.txt", filepath.Join(rootPath, "ok.txt"), "nested/good"} {
		f, err := openTerminalFile(root, rootPath, name)
		require.NoError(t, err, name)
		f.Close()
	}
	for _, name := range []string{"../ok.txt", "nested/../ok.txt", "/etc/passwd", "alias", "diralias/good", "nested", ".env", "nested/.env.local", "nested/.ssh/id_rsa", ".config/gh/hosts.yml", "nested/.codex/auth.json", "nested/.claude/.credentials.json", "nested/.git/config"} {
		_, err := openTerminalFile(root, rootPath, name)
		require.Error(t, err, name)
		var e *terminalFileError
		require.ErrorAs(t, err, &e)
		require.Equal(t, "unsafe_path", e.code, name)
	}
	big, err := os.Create(filepath.Join(rootPath, "big"))
	require.NoError(t, err)
	require.NoError(t, big.Truncate(terminalFileMaxBytes+1))
	big.Close()
	_, err = openTerminalFile(root, rootPath, "big")
	var e *terminalFileError
	require.ErrorAs(t, err, &e)
	require.Equal(t, "file_too_large", e.code)
	for _, path := range []string{"/", "/usr", "/var"} {
		_, _, err := openTerminalFileRoot(path)
		require.Error(t, err)
		require.ErrorAs(t, err, &e)
		require.Equal(t, "root_too_broad", e.code)
	}
	t.Setenv("HOME", rootPath)
	_, _, err = openTerminalFileRoot(rootPath)
	require.ErrorAs(t, err, &e)
	require.Equal(t, "root_too_broad", e.code)
}

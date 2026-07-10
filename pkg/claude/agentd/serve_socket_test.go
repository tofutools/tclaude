package agentd

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
)

func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "tc-agentd-sock-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestPrepareSocketPath(t *testing.T) {
	t.Run("refuses regular file", func(t *testing.T) {
		path := filepath.Join(shortSocketDir(t), "agentd.sock")
		require.NoError(t, os.WriteFile(path, []byte("private"), 0o600))
		err := prepareSocketPath(path, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "refusing to remove non-socket")
		got, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		assert.Equal(t, "private", string(got))
	})

	t.Run("refuses live socket", func(t *testing.T) {
		path := filepath.Join(shortSocketDir(t), "agentd.sock")
		ln, err := net.Listen("unix", path)
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })
		err = prepareSocketPath(path, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already listening")
	})

	t.Run("removes stale socket", func(t *testing.T) {
		path := filepath.Join(shortSocketDir(t), "agentd.sock")
		ln, err := net.Listen("unix", path)
		require.NoError(t, err)
		require.NoError(t, ln.Close())
		require.NoError(t, prepareSocketPath(path, false))
		_, err = os.Lstat(path)
		assert.True(t, os.IsNotExist(err))
	})

	t.Run("secure dir creates a 0700 parent", func(t *testing.T) {
		base := shortSocketDir(t)
		path := filepath.Join(base, "runtime", "agentd.sock")
		require.NoError(t, prepareSocketPath(path, true))
		fi, err := os.Stat(filepath.Dir(path))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), fi.Mode().Perm())
	})
}

func TestEnsureOwnedRuntimeDir(t *testing.T) {
	t.Run("creates missing dir 0700", func(t *testing.T) {
		dir := filepath.Join(shortSocketDir(t), "tclaude-x")
		require.NoError(t, ensureOwnedRuntimeDir(dir))
		fi, err := os.Stat(dir)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o700), fi.Mode().Perm())
	})

	t.Run("accepts an existing owned 0700 dir", func(t *testing.T) {
		dir := filepath.Join(shortSocketDir(t), "tclaude-y")
		require.NoError(t, os.Mkdir(dir, 0o700))
		require.NoError(t, ensureOwnedRuntimeDir(dir))
	})

	t.Run("refuses a group/other-writable dir", func(t *testing.T) {
		dir := filepath.Join(shortSocketDir(t), "tclaude-loose")
		require.NoError(t, os.Mkdir(dir, 0o777))
		require.NoError(t, os.Chmod(dir, 0o777)) // chmod ignores umask; make the loose state real
		err := ensureOwnedRuntimeDir(dir)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "permissions")
	})

	t.Run("refuses a non-directory", func(t *testing.T) {
		path := filepath.Join(shortSocketDir(t), "afile")
		require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
		err := ensureOwnedRuntimeDir(path)
		require.Error(t, err)
	})
}

func TestServeSocketPaths(t *testing.T) {
	home := shortSocketDir(t)
	t.Setenv("HOME", home)

	assert.Equal(t,
		[]string{SocketPath(), LegacyHomeSocketPath(), LegacySocketPath()},
		serveSocketPaths(""))

	custom := filepath.Join(home, "isolated.sock")
	assert.Equal(t, []string{custom}, serveSocketPaths(custom),
		"an explicit --socket remains an isolated override")
}

func TestConfigureServeSocketEnv(t *testing.T) {
	t.Setenv(agentipc.SocketEnv, "")
	custom := filepath.Join(shortSocketDir(t), "custom.sock")
	require.NoError(t, configureServeSocketEnv(custom))
	assert.Equal(t, custom, os.Getenv(agentipc.SocketEnv))
}

func TestListenUnixSocketsDoesNotRemoveCompetingSocket(t *testing.T) {
	dir := shortSocketDir(t)
	first := filepath.Join(dir, "first.sock")
	competing := filepath.Join(dir, "competing.sock")
	winner, err := net.Listen("unix", competing)
	require.NoError(t, err)
	t.Cleanup(func() { _ = winner.Close() })

	listeners, created, err := listenUnixSockets([]string{first, competing})
	require.Error(t, err)
	assert.Empty(t, listeners)
	assert.Empty(t, created)
	_, statErr := os.Lstat(first)
	assert.True(t, os.IsNotExist(statErr), "the loser's first socket is cleaned up")

	conn, dialErr := net.Dial("unix", competing)
	require.NoError(t, dialErr, "the winning daemon's socket must remain reachable")
	require.NoError(t, conn.Close())
}

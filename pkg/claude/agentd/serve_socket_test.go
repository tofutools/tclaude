package agentd

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareSocketPath(t *testing.T) {
	t.Run("refuses regular file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "agentd.sock")
		require.NoError(t, os.WriteFile(path, []byte("private"), 0o600))
		err := prepareSocketPath(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "refusing to remove non-socket")
		got, readErr := os.ReadFile(path)
		require.NoError(t, readErr)
		assert.Equal(t, "private", string(got))
	})

	t.Run("refuses live socket", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "agentd.sock")
		ln, err := net.Listen("unix", path)
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })
		err = prepareSocketPath(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already listening")
	})

	t.Run("removes stale socket", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "agentd.sock")
		ln, err := net.Listen("unix", path)
		require.NoError(t, err)
		require.NoError(t, ln.Close())
		require.NoError(t, prepareSocketPath(path))
		_, err = os.Lstat(path)
		assert.True(t, os.IsNotExist(err))
	})
}

func TestServeSocketPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	assert.Equal(t,
		[]string{SocketPath(), LegacySocketPath()},
		serveSocketPaths(""))

	custom := filepath.Join(home, "isolated.sock")
	assert.Equal(t, []string{custom}, serveSocketPaths(custom),
		"an explicit --socket remains an isolated override")
}

func TestListenUnixSocketsDoesNotRemoveCompetingSocket(t *testing.T) {
	dir := t.TempDir()
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

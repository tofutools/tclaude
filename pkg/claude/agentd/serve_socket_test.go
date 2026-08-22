package agentd

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc/agentipctest"
)

func shortSocketDir(t *testing.T) string {
	t.Helper()
	return agentipctest.ShortSocketDir(t)
}

func TestPrepareSocketPath(t *testing.T) {
	t.Run("refuses regular file", func(t *testing.T) {
		path := filepath.Join(shortSocketDir(t), "agentd.sock")
		require.NoError(t, os.WriteFile(path, []byte("private"), 0o600))
		err := prepareSocketPath(path)
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
		err = prepareSocketPath(path)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already listening")
	})

	t.Run("removes stale socket", func(t *testing.T) {
		path := filepath.Join(shortSocketDir(t), "agentd.sock")
		ln, err := net.Listen("unix", path)
		require.NoError(t, err)
		require.NoError(t, ln.Close())
		require.NoError(t, prepareSocketPath(path))
		_, err = os.Lstat(path)
		assert.True(t, os.IsNotExist(err))
	})
}

func TestServeSocketPaths(t *testing.T) {
	home := shortSocketDir(t)
	t.Setenv("HOME", home)

	assert.Equal(t,
		append([]string{SocketPath(), agentipc.SandboxSocketPath()}, LegacySocketPaths()...),
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

	listeners, created, err := listenUnixSockets(first, []string{competing}, failOnWarn(t))
	require.Error(t, err)
	assert.Empty(t, listeners)
	assert.Empty(t, created)
	_, statErr := os.Lstat(first)
	assert.True(t, os.IsNotExist(statErr), "the loser's first socket is cleaned up")

	conn, dialErr := net.Dial("unix", competing)
	require.NoError(t, dialErr, "the winning daemon's socket must remain reachable")
	require.NoError(t, conn.Close())
}

func failOnWarn(t *testing.T) func(string, error) {
	t.Helper()
	return func(path string, err error) {
		t.Fatalf("unexpected skip of %s: %v", path, err)
	}
}

// An operator may deliberately keep $HOME unwritable by tclaude. The canonical
// api/ socket is what agents actually dial, so an unbindable legacy alias must
// cost a warning, not the daemon.
func TestServeSocketsSkipUnbindableLegacyPaths(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory mode this test relies on to make a path unbindable")
	}
	dir := shortSocketDir(t)
	required := filepath.Join(dir, "agentd.sock")
	unwritable := filepath.Join(dir, "nowrite")
	require.NoError(t, os.Mkdir(unwritable, 0o500))
	t.Cleanup(func() { _ = os.Chmod(unwritable, 0o700) })
	legacy := filepath.Join(unwritable, "legacy.sock")

	var skipped []string
	note := func(path string, _ error) { skipped = append(skipped, path) }

	// Preparation has nothing to clean up at a path that does not exist, so an
	// unwritable parent only bites at bind time. Either way it must not be fatal.
	kept, err := prepareServeSockets(required, []string{legacy}, note)
	require.NoError(t, err, "an unbindable legacy alias must not fail startup")

	listeners, created, err := listenUnixSockets(required, kept, note)
	require.NoError(t, err, "an unbindable legacy alias must not fail startup")
	t.Cleanup(func() { closeListeners(listeners) })
	require.Len(t, listeners, 1, "the canonical socket is still bound")
	assert.Equal(t, []string{required}, created)
	assert.Equal(t, []string{legacy}, skipped, "the operator is told which alias was dropped")

	conn, dialErr := net.Dial("unix", required)
	require.NoError(t, dialErr, "the canonical socket must be reachable")
	require.NoError(t, conn.Close())
}

// AF_UNIX bind reports EADDRINUSE for ANY pre-existing directory entry, so the
// errno cannot stand in for "a second daemon". Something landing at an alias
// between the prepare pass and the bind pass — a dotfile manager or a home
// restore recreating it — must still only cost the alias, since the canonical
// socket agents dial is already up.
func TestListenUnixSocketsSkipsDeadEntryAppearingAtLegacyPath(t *testing.T) {
	dir := shortSocketDir(t)
	required := filepath.Join(dir, "agentd.sock")
	legacy := filepath.Join(dir, "legacy.sock")

	for name, occupy := range map[string]func(t *testing.T){
		"regular file": func(t *testing.T) {
			require.NoError(t, os.WriteFile(legacy, []byte("not a socket"), 0o600))
		},
		"socket left by an abrupt exit": func(t *testing.T) {
			ln, err := net.Listen("unix", filepath.Join(dir, "tmp.sock"))
			require.NoError(t, err)
			// Rename out from under Go so Close() cannot unlink it: a socket
			// inode with nothing accepting on it, exactly what a killed daemon
			// leaves behind.
			require.NoError(t, os.Rename(filepath.Join(dir, "tmp.sock"), legacy))
			require.NoError(t, ln.Close())
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Cleanup(func() { _ = os.Remove(legacy); _ = os.Remove(required) })
			occupy(t)

			var skipped []string
			listeners, created, err := listenUnixSockets(required, []string{legacy},
				func(path string, _ error) { skipped = append(skipped, path) })
			require.NoError(t, err, "a dead entry at an alias must not fail startup")
			t.Cleanup(func() { closeListeners(listeners) })
			require.Len(t, listeners, 1, "the canonical socket is still bound")
			assert.Equal(t, []string{required}, created)
			assert.Equal(t, []string{legacy}, skipped)

			conn, dialErr := net.Dial("unix", required)
			require.NoError(t, dialErr, "the canonical socket must be reachable")
			require.NoError(t, conn.Close())
		})
	}
}

// A live daemon on a legacy alias is a second runtime, not an unavailable
// convenience, so it stays fatal even though the alias is best-effort.
func TestPrepareServeSocketsFailsOnLiveLegacySocket(t *testing.T) {
	dir := shortSocketDir(t)
	required := filepath.Join(dir, "agentd.sock")
	legacy := filepath.Join(dir, "legacy.sock")
	ln, err := net.Listen("unix", legacy)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	kept, err := prepareServeSockets(required, []string{legacy}, failOnWarn(t))
	require.Error(t, err)
	assert.ErrorIs(t, err, errSocketInUse)
	assert.Empty(t, kept)
}

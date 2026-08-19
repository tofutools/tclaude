package agent

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc/agentipctest"
)

// A socket override short-circuits every fallback, so a stale or mistyped one
// makes a running daemon look absent. The error has to name the path it dialled
// and where that path came from, or the operator goes hunting a daemon that is
// already up.
func TestDaemonUnreachableMsgNamesTheSocketOverride(t *testing.T) {
	t.Run("no override keeps the plain not-running message", func(t *testing.T) {
		t.Setenv(agentipc.SocketEnv, "")
		require.Equal(t, daemonRequiredMsg, daemonUnreachableMsg())
	})

	t.Run("override points at a dead path while the default is live", func(t *testing.T) {
		home := agentipctest.ShortSocketDir(t)
		t.Setenv("HOME", home)

		canonical := agentipc.CanonicalSocketPath()
		require.NoError(t, os.MkdirAll(filepath.Dir(canonical), 0o755))
		ln, err := net.Listen("unix", canonical)
		require.NoError(t, err)
		t.Cleanup(func() { _ = ln.Close() })

		dead := filepath.Join(home, "nonexistent.sock")
		t.Setenv(agentipc.SocketEnv, dead)

		msg := daemonUnreachableMsg()
		require.Contains(t, msg, dead, "names the path actually dialled")
		require.Contains(t, msg, agentipc.SocketEnv, "names where that path came from")
		require.Contains(t, msg, canonical, "names the socket that IS live")
		require.Contains(t, msg, "Unset", "names the fix")
		require.NotContains(t, msg, "is not running",
			"a daemon is running; saying otherwise is what sent the operator hunting")
	})

	t.Run("override points at a dead path and nothing else is live", func(t *testing.T) {
		home := agentipctest.ShortSocketDir(t)
		t.Setenv("HOME", home)
		dead := filepath.Join(home, "nonexistent.sock")
		t.Setenv(agentipc.SocketEnv, dead)

		msg := daemonUnreachableMsg()
		require.Contains(t, msg, dead)
		require.Contains(t, msg, "tclaude agentd serve --socket "+dead,
			"starting a daemon on the pinned socket is the fix when nothing is live")
	})
}

func TestRealDaemonAvailableFallsBackToLegacySocket(t *testing.T) {
	home := agentipctest.ShortSocketDir(t)
	t.Setenv("HOME", home)
	t.Setenv(agentipc.SocketEnv, "")

	legacy := agentipc.LegacySocketPath()
	require.NoError(t, os.MkdirAll(filepath.Dir(legacy), 0o755))
	ln, err := net.Listen("unix", legacy)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	require.True(t, realDaemonAvailable())
}

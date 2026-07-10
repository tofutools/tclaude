package agent

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
)

func TestRealDaemonAvailableFallsBackToLegacySocket(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(agentipc.SocketEnv, "")

	legacy := agentipc.LegacySocketPath()
	require.NoError(t, os.MkdirAll(filepath.Dir(legacy), 0o755))
	ln, err := net.Listen("unix", legacy)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	require.True(t, realDaemonAvailable())
}

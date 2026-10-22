//go:build linux

package session

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

// The production launch has already opened tclaude's private data directory
// and an isolated agent launch has a real agentd socket. The direct host smokes
// bypass those earlier seams, so materialize only that production
// prerequisite; the engines under test remain the real binaries.
func prepareSmokeControlPlane(t *testing.T) {
	t.Helper()
	require.NoError(t, os.MkdirAll(tclcommon.TclaudeDataDir(), 0o700))
	socketPath := agentipc.CanonicalSocketPath()
	if _, err := os.Lstat(socketPath); err == nil {
		return
	} else {
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	require.NoError(t, os.MkdirAll(filepath.Dir(socketPath), 0o700))
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	})
}

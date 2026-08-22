package agentipc

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc/agentipctest"
)

func TestSocketPaths(t *testing.T) {
	home := t.TempDir()
	agentipctest.IsolateSocketEnv(t)
	t.Setenv("HOME", home)

	assert.Equal(t, filepath.Join(home, ".tclaude", "api", "agentd-socket"), CanonicalSocketDir())
	assert.Equal(t, filepath.Join(home, ".tclaude", "api", "agentd-socket", "agentd.sock"), CanonicalSocketPath())
	assert.Equal(t, filepath.Join(home, ".tclaude", "api", "agentd.sock"), LegacyAPISocketPath())
	assert.Equal(t, filepath.Join(home, ".tclaude-agentd.sock"), LegacyHomeSocketPath())
	assert.Equal(t, filepath.Join(home, ".tclaude", "agentd.sock"), LegacySocketPath())
	assert.Equal(t, []string{LegacyAPISocketPath(), LegacyHomeSocketPath(), LegacySocketPath()}, LegacySocketPaths())
	assert.Equal(t, CanonicalSocketPath(), ClientSocketPath())
	assert.Equal(t,
		[]string{CanonicalSocketPath(), LegacyAPISocketPath(), LegacyHomeSocketPath(), LegacySocketPath()},
		ClientSocketPaths())

	override := filepath.Join(home, "agent.sock")
	t.Setenv(SocketEnv, override)
	assert.Equal(t, override, ClientSocketPath())
	assert.Equal(t, []string{override}, ClientSocketPaths())
	assert.Equal(t, override, ExplicitSocketPath())

	t.Setenv(SocketEnv, "relative.sock")
	assert.Equal(t, CanonicalSocketPath(), ClientSocketPath())
	assert.Empty(t, ExplicitSocketPath())
}

func TestLiveCanonicalSocketPathRejectsSymlinkedParent(t *testing.T) {
	home := agentipctest.ShortSocketDir(t)
	t.Setenv("HOME", home)
	target := agentipctest.ShortSocketDir(t)
	require.NoError(t, os.MkdirAll(filepath.Dir(CanonicalSocketDir()), 0o700))
	require.NoError(t, os.Symlink(target, CanonicalSocketDir()))
	listener, err := net.Listen("unix", filepath.Join(target, "agentd.sock"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	assert.False(t, CanonicalSocketDirAvailable())
	assert.False(t, LiveCanonicalSocketPath(),
		"a live socket beneath a symlinked parent is not the dedicated projection")
}

func TestLiveCanonicalSocketPathRejectsPeerWritableParent(t *testing.T) {
	home := agentipctest.ShortSocketDir(t)
	t.Setenv("HOME", home)
	dir := CanonicalSocketDir()
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.Chmod(dir, 0o777),
		"chmod after mkdir avoids the process umask masking the unsafe bits")
	listener, err := net.Listen("unix", CanonicalSocketPath())
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	assert.False(t, CanonicalSocketDirAvailable())
	assert.False(t, LiveCanonicalSocketPath(),
		"a peer-writable parent permits replacement of the managed endpoint")
}

func TestLiveSocketPathRejectsNonSocketAndSymlink(t *testing.T) {
	dir := agentipctest.ShortSocketDir(t)
	livePath := filepath.Join(dir, "live.sock")
	listener, err := net.Listen("unix", livePath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	assert.True(t, LiveSocketPath(livePath))

	regular := filepath.Join(dir, "regular")
	require.NoError(t, os.WriteFile(regular, []byte("not a socket"), 0o600))
	assert.False(t, LiveSocketPath(regular))

	symlink := filepath.Join(dir, "linked.sock")
	require.NoError(t, os.Symlink(livePath, symlink))
	assert.False(t, LiveSocketPath(symlink))
}

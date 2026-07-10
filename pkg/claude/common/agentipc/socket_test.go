package agentipc

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCanonicalSocketPath_TmpPerUid(t *testing.T) {
	// The canonical socket is always /tmp/tclaude-<uid>/agentd.sock — the literal
	// /tmp, independent of $XDG_RUNTIME_DIR and $TMPDIR, so every process on the
	// host agrees.
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/9999")
	t.Setenv("TMPDIR", t.TempDir())

	want := filepath.Join("/tmp", fmt.Sprintf("tclaude-%d", os.Getuid()), socketBasename)
	assert.Equal(t, want, CanonicalSocketPath())
	assert.Equal(t, want, ClientSocketPath())

	// Unsetting XDG changes nothing.
	t.Setenv("XDG_RUNTIME_DIR", "")
	assert.Equal(t, want, CanonicalSocketPath())
}

func TestLegacySocketPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	assert.Equal(t, filepath.Join(home, ".tclaude-agentd.sock"), LegacyHomeSocketPath())
	assert.Equal(t, filepath.Join(home, ".tclaude", "agentd.sock"), LegacySocketPath())
}

func TestClientSocketPaths_CandidateOrder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(SocketEnv, "")

	got := ClientSocketPaths()
	// Canonical (runtime dir) first, then both legacy home paths for the
	// migration window.
	assert.Equal(t, []string{
		CanonicalSocketPath(),
		LegacyHomeSocketPath(),
		LegacySocketPath(),
	}, got)
}

func TestExplicitSocketOverride(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	override := filepath.Join(home, "agent.sock")
	t.Setenv(SocketEnv, override)
	assert.Equal(t, override, ClientSocketPath())
	assert.Equal(t, []string{override}, ClientSocketPaths())
	assert.Equal(t, override, ExplicitSocketPath())

	// A non-absolute override is ignored (never an ambient-CWD lookup).
	t.Setenv(SocketEnv, "relative.sock")
	assert.Equal(t, CanonicalSocketPath(), ClientSocketPath())
	assert.Empty(t, ExplicitSocketPath())
}

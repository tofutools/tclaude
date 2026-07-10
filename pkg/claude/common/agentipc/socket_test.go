package agentipc

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSocketPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	assert.Equal(t, filepath.Join(home, ".tclaude-agentd.sock"), CanonicalSocketPath())
	assert.Equal(t, filepath.Join(home, ".tclaude", "agentd.sock"), LegacySocketPath())
	assert.Equal(t, CanonicalSocketPath(), ClientSocketPath())
	assert.Equal(t, []string{CanonicalSocketPath(), LegacySocketPath()}, ClientSocketPaths())

	override := filepath.Join(home, "agent.sock")
	t.Setenv(SocketEnv, override)
	assert.Equal(t, override, ClientSocketPath())
	assert.Equal(t, []string{override}, ClientSocketPaths())

	t.Setenv(SocketEnv, "relative.sock")
	assert.Equal(t, CanonicalSocketPath(), ClientSocketPath())
}

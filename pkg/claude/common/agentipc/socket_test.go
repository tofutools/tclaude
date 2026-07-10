package agentipc

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSocketPaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	assert.Equal(t, filepath.Join(home, ".tclaude", "agentd.sock"), OperatorSocketPath())
	assert.Equal(t, filepath.Join(home, ".tclaude-agentd.sock"), SandboxedAgentSocketPath())
	assert.Equal(t, OperatorSocketPath(), ClientSocketPath())

	override := filepath.Join(home, "agent.sock")
	t.Setenv(SocketEnv, override)
	assert.Equal(t, override, ClientSocketPath())

	t.Setenv(SocketEnv, "relative.sock")
	assert.Equal(t, OperatorSocketPath(), ClientSocketPath())
}

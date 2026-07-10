package session

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestApplyCodexAgentSocketEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	env := map[string]string{}
	require.NoError(t, ApplyCodexAgentSocketEnv("", env))
	assert.NotContains(t, env, agentipc.SocketEnv)

	require.NoError(t, ApplyCodexAgentSocketEnv(harness.CodexAgentProfile, env))
	assert.Equal(t, filepath.Join(home, ".tclaude-agentd.sock"), env[agentipc.SocketEnv])
}

func TestApplyCodexAgentSocketEnvRequiresRestartForLegacyOnlyDaemon(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Dir(agentipc.LegacySocketPath()), 0o755))
	legacy, err := net.Listen("unix", agentipc.LegacySocketPath())
	require.NoError(t, err)
	t.Cleanup(func() { _ = legacy.Close() })

	err = ApplyCodexAgentSocketEnv(harness.CodexAgentProfile, map[string]string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "restart agentd")
	assert.Contains(t, err.Error(), agentipc.LegacySocketPath())
}

func TestApplyCodexAgentSocketEnvAcceptsCanonicalDaemon(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	canonical, err := net.Listen("unix", agentipc.CanonicalSocketPath())
	require.NoError(t, err)
	t.Cleanup(func() { _ = canonical.Close() })

	env := map[string]string{}
	require.NoError(t, ApplyCodexAgentSocketEnv(harness.CodexAgentProfile, env))
	assert.Equal(t, agentipc.CanonicalSocketPath(), env[agentipc.SocketEnv])
}

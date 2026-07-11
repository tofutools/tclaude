package session

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc/agentipctest"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestApplyAgentSocketEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(agentipc.SocketEnv, "")

	env := map[string]string{}
	require.NoError(t, ApplyAgentSocketEnv(harness.DefaultName, harness.ClaudeSandboxInherit, "", env))
	assert.NotContains(t, env, agentipc.SocketEnv)

	for _, tc := range []struct {
		name       string
		harness    string
		sandbox    string
		permission string
	}{
		{"managed Codex", harness.CodexName, "", harness.CodexAgentProfile},
		{"forced-on Claude", harness.DefaultName, harness.ClaudeSandboxOn, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			require.NoError(t, ApplyAgentSocketEnv(tc.harness, tc.sandbox, tc.permission, env))
			assert.Equal(t, filepath.Join(home, ".tclaude", "api", "agentd.sock"), env[agentipc.SocketEnv])
		})
	}
}

func TestApplyAgentSocketEnvRequiresRestartForLegacyOnlyDaemon(t *testing.T) {
	home := agentipctest.ShortSocketDir(t)
	t.Setenv("HOME", home)
	t.Setenv(agentipc.SocketEnv, "")
	require.NoError(t, os.MkdirAll(filepath.Dir(agentipc.LegacySocketPath()), 0o755))
	legacy, err := net.Listen("unix", agentipc.LegacySocketPath())
	require.NoError(t, err)
	t.Cleanup(func() { _ = legacy.Close() })

	for _, tc := range []struct {
		name       string
		harness    string
		sandbox    string
		permission string
	}{
		{"managed Codex", harness.CodexName, "", harness.CodexAgentProfile},
		{"forced-on Claude", harness.DefaultName, harness.ClaudeSandboxOn, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ApplyAgentSocketEnv(tc.harness, tc.sandbox, tc.permission, map[string]string{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "restart agentd")
			assert.Contains(t, err.Error(), agentipc.LegacySocketPath())
		})
	}
}

func TestApplyAgentSocketEnvAcceptsCanonicalDaemon(t *testing.T) {
	home := agentipctest.ShortSocketDir(t)
	t.Setenv("HOME", home)
	t.Setenv(agentipc.SocketEnv, "")
	require.NoError(t, os.MkdirAll(filepath.Dir(agentipc.CanonicalSocketPath()), 0o700))
	canonical, err := net.Listen("unix", agentipc.CanonicalSocketPath())
	require.NoError(t, err)
	t.Cleanup(func() { _ = canonical.Close() })

	env := map[string]string{}
	require.NoError(t, ApplyAgentSocketEnv(
		harness.CodexName, "", harness.CodexAgentProfile, env))
	assert.Equal(t, agentipc.CanonicalSocketPath(), env[agentipc.SocketEnv])
}

func TestApplyAgentSocketEnvRejectsCustomSocket(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	custom := filepath.Join(home, "custom.sock")
	t.Setenv(agentipc.SocketEnv, custom)

	err := ApplyAgentSocketEnv(
		harness.CodexName, "", harness.CodexAgentProfile, map[string]string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "custom socket")
	assert.Contains(t, err.Error(), agentipc.CanonicalSocketPath())
}

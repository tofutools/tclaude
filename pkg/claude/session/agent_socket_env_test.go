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

// skipIfCanonicalDaemon skips a test that needs the canonical socket path free
// or unreachable when a real daemon is already answering on it (the fixed
// per-uid /tmp path can collide with a live daemon on a developer box).
func skipIfCanonicalDaemon(t *testing.T) {
	t.Helper()
	if agentipc.SocketReachable(agentipc.CanonicalSocketPath()) {
		t.Skip("a daemon is already listening on the canonical socket; skipping to avoid collision")
	}
}

func TestApplyAgentSocketEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(agentipc.SocketEnv, "")
	skipIfCanonicalDaemon(t)

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
			// The pinned socket is the canonical runtime-dir path, never a home path.
			assert.Equal(t, agentipc.CanonicalSocketPath(), env[agentipc.SocketEnv])
			assert.NotContains(t, env[agentipc.SocketEnv], home)
		})
	}
}

func TestApplyAgentSocketEnvRequiresRestartForLegacyOnlyDaemon(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "tc-session-sock-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	t.Setenv(agentipc.SocketEnv, "")
	skipIfCanonicalDaemon(t)

	// Only a legacy home socket is answering.
	require.NoError(t, os.MkdirAll(filepath.Dir(agentipc.LegacyHomeSocketPath()), 0o755))
	legacy, err := net.Listen("unix", agentipc.LegacyHomeSocketPath())
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
			assert.Contains(t, err.Error(), agentipc.CanonicalSocketPath())
		})
	}
}

func TestApplyAgentSocketEnvAcceptsCanonicalDaemon(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "tc-session-sock-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	t.Setenv("HOME", home)
	t.Setenv(agentipc.SocketEnv, "")
	skipIfCanonicalDaemon(t)

	canonical := agentipc.CanonicalSocketPath()
	require.NoError(t, os.MkdirAll(filepath.Dir(canonical), 0o700))
	_ = os.Remove(canonical) // clear a stale socket file, if any
	ln, err := net.Listen("unix", canonical)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	env := map[string]string{}
	require.NoError(t, ApplyAgentSocketEnv(
		harness.CodexName, "", harness.CodexAgentProfile, env))
	assert.Equal(t, canonical, env[agentipc.SocketEnv])
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

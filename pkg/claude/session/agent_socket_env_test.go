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
	require.NoError(t, ApplyAgentSocketEnv(harness.DefaultName, harness.ClaudeSandboxInherit, "", false, env))
	assert.NotContains(t, env, agentipc.SocketEnv)

	for _, tc := range []struct {
		name       string
		harness    string
		sandbox    string
		permission string
		isolated   bool
	}{
		{"managed Codex", harness.CodexName, "", harness.CodexAgentProfile, false},
		{"forced-on Claude", harness.DefaultName, harness.ClaudeSandboxOn, "", false},
		{"isolated tclaude layer", harness.CodexName, harness.SandboxDangerFull, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			require.NoError(t, ApplyAgentSocketEnv(tc.harness, tc.sandbox, tc.permission, tc.isolated, env))
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
			err := ApplyAgentSocketEnv(tc.harness, tc.sandbox, tc.permission, false, map[string]string{})
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
		harness.CodexName, "", harness.CodexAgentProfile, false, env))
	assert.Equal(t, agentipc.CanonicalSocketPath(), env[agentipc.SocketEnv])
}

func TestApplyAgentSocketEnvSelectsOnlyLiveDirectStableSocket(t *testing.T) {
	for _, tc := range []struct {
		name   string
		occupy func(t *testing.T, stable, canonical string)
		stable bool
	}{
		{
			name: "live socket",
			occupy: func(t *testing.T, stable, _ string) {
				listener, err := net.Listen("unix", stable)
				require.NoError(t, err)
				t.Cleanup(func() { _ = listener.Close() })
			},
			stable: true,
		},
		{
			name: "regular file",
			occupy: func(t *testing.T, stable, _ string) {
				require.NoError(t, os.WriteFile(stable, []byte("not a socket"), 0o600))
			},
		},
		{
			name: "directory",
			occupy: func(t *testing.T, stable, _ string) {
				require.NoError(t, os.Mkdir(stable, 0o700))
			},
		},
		{
			name: "symlink to live canonical",
			occupy: func(t *testing.T, stable, canonical string) {
				require.NoError(t, os.Symlink(canonical, stable))
			},
		},
		{
			name: "stale socket",
			occupy: func(t *testing.T, stable, _ string) {
				listener, err := net.Listen("unix", stable)
				require.NoError(t, err)
				listener.(*net.UnixListener).SetUnlinkOnClose(false)
				require.NoError(t, listener.Close())
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := agentipctest.ShortSocketDir(t)
			t.Setenv("HOME", home)
			t.Setenv(agentipc.SocketEnv, "")
			canonical := agentipc.CanonicalSocketPath()
			stable := agentipc.SandboxSocketPath()
			require.NoError(t, os.MkdirAll(filepath.Dir(stable), 0o700))
			canonicalListener, err := net.Listen("unix", canonical)
			require.NoError(t, err)
			t.Cleanup(func() { _ = canonicalListener.Close() })
			tc.occupy(t, stable, canonical)

			env := map[string]string{}
			require.NoError(t, ApplyAgentSocketEnv(
				harness.CodexName, harness.SandboxDangerFull, "", true, env))
			want := canonical
			if tc.stable {
				want = stable
			}
			assert.Equal(t, want, env[agentipc.SocketEnv])
		})
	}
}

func TestApplyAgentSocketEnvRejectsCustomSocket(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	custom := filepath.Join(home, "custom.sock")
	t.Setenv(agentipc.SocketEnv, custom)

	err := ApplyAgentSocketEnv(
		harness.CodexName, "", harness.CodexAgentProfile, false, map[string]string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "custom socket")
	assert.Contains(t, err.Error(), agentipc.CanonicalSocketPath())
}

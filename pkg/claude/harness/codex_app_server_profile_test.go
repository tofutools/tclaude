package harness

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestCodexAppServerProfileOverridesCarriesCompleteManagedPosture(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("TCLAUDE_AGENTD_SOCKET", filepath.Join(home, ".tclaude", "api", "agentd.sock"))
	tmuxBase := filepath.Join(home, "tmux-private")
	require.NoError(t, os.MkdirAll(tmuxBase, 0o700))
	t.Setenv("TMUX_TMPDIR", tmuxBase)
	socketDir, err := os.MkdirTemp("/tmp", "tcl1151-sock-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	materialized := []string{filepath.Join(socketDir, "service.sock")}
	listener, err := net.Listen("unix", materialized[0])
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	name, path, err := EnsureCodexAgentLaunchProfileForRules(CodexSandboxRules{
		ReadDirs:  []string{filepath.Join(home, "read")},
		WriteDirs: []string{filepath.Join(home, "repo", ".git")},
		DenyDirs:  []string{filepath.Join(home, "secret")},
		UnixSockets: &sandboxpolicy.UnixSocketRules{
			Mode:  sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.SocketAllowEntry{{Path: materialized[0]}},
		},
		MaterializedUnixSocketPaths: &materialized,
		RequireSplitPolicy:          true,
	}, sandboxpolicy.NetworkAccessInternet, "1234567890abcdef")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.Remove(path) })

	overrides, err := CodexAppServerProfileOverrides(path)
	require.NoError(t, err)
	joined := strings.Join(overrides, "\n")
	assert.Contains(t, joined, `default_permissions="`+name+`"`)
	assert.Contains(t, joined, `extends=":workspace"`)
	for _, want := range []string{
		filepath.Join(home, "read") + `"="read"`,
		filepath.Join(home, "repo", ".git") + `"="write"`,
		filepath.Join(home, "secret") + `"="none"`,
		filepath.Join(home, ".tclaude", "data") + `"="none"`,
		filepath.Join(home, ".claude", "sessions") + `"="none"`,
		filepath.Join(tmuxBase, fmt.Sprintf("tmux-%d", os.Getuid())) + `"="none"`,
		filepath.Join(home, ".tclaude", "api", "agentd.sock") + `"="allow"`,
		materialized[0] + `"="allow"`,
		`network={enabled=true`,
		`features.network_proxy=false`,
		`features.use_legacy_landlock=false`,
	} {
		assert.Contains(t, joined, want)
	}
}

func TestCodexSpawnerAppServerMirrorsExecutionPosture(t *testing.T) {
	spec := SpawnSpec{
		PermissionProfile: "tclaude-agent-1234567890abcdef",
		CodexAppServerProfileOverrides: []string{
			`default_permissions="tclaude-agent-1234567890abcdef"`,
			`permissions.tclaude-agent-1234567890abcdef={extends=":workspace",filesystem={},network={enabled=true,unix_sockets={}}}`,
		},
		StrongNestedSandbox:        true,
		ApprovalPolicy:             "never",
		BypassHookTrust:            true,
		FastMode:                   FastModeOn,
		ShellEnvironment:           map[string]string{"GOBIN": "/tmp/go bin"},
		CodexAppServerSocket:       "/tmp/app.sock",
		CodexAppServerURL:          "ws://127.0.0.1:45678",
		CodexAppServerTokenSHA256:  strings.Repeat("ab", 32),
		CodexAppServerTokenHandoff: "/tmp/token",
		TclaudeExecutable:          "/opt/tclaude",
		CodexAppServerPIDFile:      "/tmp/server.pid",
		CodexAppServerLogFile:      "/tmp/server.log",
	}
	got := codexSpawner{}.BuildCommand(spec)
	server := strings.Split(got, " app-server --listen")[0]
	for _, want := range []string{
		`--dangerously-bypass-hook-trust`,
		`default_permissions="tclaude-agent-1234567890abcdef"`,
		`features.use_legacy_landlock=false`,
		`--ask-for-approval never`,
		`service_tier="fast"`,
		`shell_environment_policy.set.GOBIN="/tmp/go bin"`,
	} {
		assert.Contains(t, server, want)
	}
	assert.NotContains(t, server, " -p ")
	assert.Contains(t, got, "-p tclaude-agent-1234567890abcdef")
}

func TestCodexAppServerProfileOverridesRejectsUnknownProfileFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "profile.toml")
	require.NoError(t, os.WriteFile(path, []byte(`
default_permissions = "test"
[permissions.test]
extends = ":workspace"
future_authority = true
[permissions.test.filesystem]
"/tmp" = "read"
[permissions.test.network]
enabled = false
[permissions.test.network.unix_sockets]
`), 0o600))
	_, err := CodexAppServerProfileOverrides(path)
	require.Error(t, err)
}

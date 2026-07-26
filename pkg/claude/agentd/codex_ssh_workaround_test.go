package agentd

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestCodexSSHWorkaroundCopiesSystemConfigAndPinsGit(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the Codex managed-sandbox SSH workaround is Linux-only")
	}
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	systemRoot := t.TempDir()
	systemConfig := filepath.Join(systemRoot, "ssh_config")
	dropInSource := filepath.Join(systemRoot, codexSSHDropInDirName)
	require.NoError(t, os.MkdirAll(dropInSource, 0o700))
	proxySource := filepath.Join(systemRoot, "systemd-proxy.conf")
	require.NoError(t, os.WriteFile(proxySource, []byte(
		"Host *.example.test\n  ProxyCommand /usr/lib/systemd/systemd-ssh-proxy unix %h %p\n"), 0o644))
	require.NoError(t, os.Symlink(proxySource, filepath.Join(dropInSource, "20-systemd-ssh-proxy.conf")))
	require.NoError(t, os.WriteFile(systemConfig, []byte(
		"Include "+dropInSource+"/*.conf\nHost *\n  SendEnv LANG\n"), 0o644))

	oldSystemConfig, oldLookPath := codexSSHSystemConfig, codexSSHLookPath
	codexSSHSystemConfig = systemConfig
	codexSSHLookPath = exec.LookPath
	t.Cleanup(func() {
		codexSSHSystemConfig = oldSystemConfig
		codexSSHLookPath = oldLookPath
	})

	got, cleanup, err := prepareCodexSSHWorkaroundForNewLaunch(
		sandboxpolicy.EmptySnapshot(), "spwn-codex-ssh-test", true)
	require.NoError(t, err)
	t.Cleanup(cleanup)

	env := map[string]string{}
	for _, entry := range got.Effective.Environment {
		env[entry.Name] = entry.Value
	}
	configDir := env[codexSSHAgentDirectory]
	require.NotEmpty(t, configDir)
	configPath := filepath.Join(configDir, codexSSHConfigName)
	command := env[codexSSHCommandEnv]
	assert.Contains(t, command, " -F ")
	assert.Contains(t, command, configPath)

	contents, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(string(contents), "Include ~/.ssh/config\n"),
		"the operator's user config must retain first-value precedence")
	assert.Contains(t, string(contents), filepath.Join(configDir, codexSSHDropInDirName)+"/*.conf")
	assert.NotContains(t, string(contents), dropInSource+"/*.conf")

	copiedProxy := filepath.Join(configDir, codexSSHDropInDirName, "20-systemd-ssh-proxy.conf")
	info, err := os.Lstat(copiedProxy)
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular(), "system-owned symlinks are copied as private regular files")
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	proxyContents, err := os.ReadFile(copiedProxy)
	require.NoError(t, err)
	assert.Contains(t, string(proxyContents), "systemd-ssh-proxy")

	configInfo, err := os.Stat(configPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), configInfo.Mode().Perm())

	disabled, err := configureCodexSSHWorkaroundDeclaration(got, false)
	require.NoError(t, err)
	for _, name := range disabled.Effective.AgentDirectories {
		assert.NotEqual(t, codexSSHAgentDirectory, name)
	}
	for _, entry := range disabled.Effective.Environment {
		assert.NotEqual(t, codexSSHAgentDirectory, entry.Name)
		assert.NotEqual(t, codexSSHCommandEnv, entry.Name)
	}
}

func TestCodexSSHWorkaroundDisabledLeavesSnapshotUnchanged(t *testing.T) {
	snapshot := sandboxpolicy.EmptySnapshot()
	got, cleanup, err := prepareCodexSSHWorkaroundForNewLaunch(snapshot, "spwn-codex-ssh-off", false)
	require.NoError(t, err)
	t.Cleanup(cleanup)
	assert.Equal(t, snapshot, got)
}

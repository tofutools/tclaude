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
	userRoot := t.TempDir()
	userConfig := filepath.Join(userRoot, "config")
	require.NoError(t, os.WriteFile(userConfig, []byte(
		"Match host user-only.example.test\n  Port 2299\n"), 0o600))
	systemConfig := filepath.Join(systemRoot, "ssh_config")
	dropInSource := filepath.Join(systemRoot, codexSSHDropInDirName)
	require.NoError(t, os.MkdirAll(dropInSource, 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(systemRoot, "nested"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(systemRoot, "nested", "common.conf"), []byte(
		"Host *\n  User mirrored-system-user\n"), 0o644))
	proxySource := filepath.Join(systemRoot, "systemd-proxy.conf")
	require.NoError(t, os.WriteFile(proxySource, []byte(
		"Host git.example.test\n  Port 2201\n  ProxyCommand /usr/lib/systemd/systemd-ssh-proxy unix %h %p\n"+
			"  Include nested/common.conf\n"), 0o644))
	require.NoError(t, os.Symlink(proxySource, filepath.Join(dropInSource, "20-systemd-ssh-proxy.conf")))
	require.NoError(t, os.WriteFile(systemConfig, []byte(
		"Include ssh_config.d/*.conf\nHost *\n  SendEnv LANG\n"), 0o644))

	oldSystemConfig, oldUserConfig, oldLookPath := codexSSHSystemConfig, codexSSHUserConfig, codexSSHLookPath
	codexSSHSystemConfig = systemConfig
	codexSSHUserConfig = userConfig
	codexSSHLookPath = exec.LookPath
	t.Cleanup(func() {
		codexSSHSystemConfig = oldSystemConfig
		codexSSHUserConfig = oldUserConfig
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
	assert.True(t, strings.HasPrefix(string(contents), "Include "+userConfig+"\nHost *\n"),
		"the operator's user config must retain first-value precedence")
	assert.NotContains(t, string(contents), dropInSource+"/*.conf")

	copiedProxy := filepath.Join(configDir, codexSSHDropInDirName, "20-systemd-ssh-proxy.conf")
	assert.Contains(t, string(contents), "Include "+copiedProxy)
	info, err := os.Lstat(copiedProxy)
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular(), "system-owned symlinks are copied as private regular files")
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	proxyContents, err := os.ReadFile(copiedProxy)
	require.NoError(t, err)
	assert.Contains(t, string(proxyContents), "systemd-ssh-proxy")
	assert.Contains(t, string(proxyContents), filepath.Join(configDir, "includes"),
		"relative nested includes are mirrored instead of resolving under ~/.ssh")

	configInfo, err := os.Stat(configPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), configInfo.Mode().Perm())

	// The private tree is self-contained even after the source system config is
	// gone. ssh -G behaviorally proves that a user config ending inside a
	// nonmatching Host block cannot suppress the mirrored system drop-in.
	require.NoError(t, os.Rename(systemRoot, systemRoot+".gone"))
	sshPath, err := exec.LookPath("ssh")
	require.NoError(t, err)
	output, err := exec.Command(sshPath, "-F", configPath, "-G", "git.example.test").CombinedOutput()
	require.NoErrorf(t, err, "ssh -G output:\n%s", output)
	assert.Regexp(t, `(?m)^port 2201$`, string(output))
	assert.Regexp(t, `(?m)^user mirrored-system-user$`, string(output))

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

func TestCodexSSHWorkaroundProfileClampsUnsupportedSandbox(t *testing.T) {
	on := true
	profile, fail := buildProfileFromJSON(spawnProfileJSON{
		Name: "raw-codex", Harness: "codex", Sandbox: "workspace-write",
		SSHWorkaround: &on,
	})
	require.Nil(t, fail)
	require.NotNil(t, profile)
	require.NotNil(t, profile.SSHWorkaround)
	assert.False(t, *profile.SSHWorkaround)
}

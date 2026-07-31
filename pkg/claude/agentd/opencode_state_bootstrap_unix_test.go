//go:build linux || darwin

package agentd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

// The read-only config bootstrap is platform-parameterized precisely so both
// platforms' behavior is observable from either host: a darwin-only test file
// would leave the Linux path (TCL-892) unexercised in CI, which is how the gap
// survived in the first place.
func TestPrepareOpenCodeReadOnlyConfigBootstrapsWithoutOverwrite(t *testing.T) {
	for _, platform := range []string{"Linux", "Darwin"} {
		t.Run(platform, func(t *testing.T) {
			root := t.TempDir()
			configDir := filepath.Join(t.TempDir(), "opencode")
			require.NoError(t, os.Mkdir(configDir, 0o700))
			spec := openCodeConfigBootstrapSpec(root, configDir, configDir)

			require.NoError(t, prepareOpenCodeReadOnlyConfig(spec, platform))
			path := filepath.Join(configDir, openCodeInstallBootstrapFile)
			raw, err := os.ReadFile(path)
			require.NoError(t, err)
			// Content, not merely existence: a payload OpenCode would rewrite
			// leaves a dirty diff in the operator's own dotfiles.
			assert.Equal(t, openCodeInstallGitignore, string(raw))

			require.NoError(t, os.WriteFile(path, []byte("operator-owned"), 0o640))
			require.NoError(t, prepareOpenCodeReadOnlyConfig(spec, platform))
			raw, err = os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, "operator-owned", string(raw),
				"the pre-wall bootstrap must never overwrite existing host config metadata")
		})
	}
}

// The Linux private-state layout binds the AMBIENT config directory onto the
// per-agent one whenever an ambient ~/.config/opencode exists, so the file has
// to land in the bind's source — the per-agent target is only what the sandbox
// sees the source through.
func TestPrepareOpenCodeReadOnlyConfigSeedsProjectionSource(t *testing.T) {
	root := t.TempDir()
	ambientConfig := filepath.Join(t.TempDir(), "opencode")
	require.NoError(t, os.Mkdir(ambientConfig, 0o700))
	privateConfig := filepath.Join(root, "config", "opencode")
	require.NoError(t, os.MkdirAll(privateConfig, 0o700))

	spec := openCodeConfigBootstrapSpec(root, privateConfig, ambientConfig)
	require.NoError(t, prepareOpenCodeReadOnlyConfig(spec, "Linux"))

	raw, err := os.ReadFile(filepath.Join(ambientConfig, openCodeInstallBootstrapFile))
	require.NoError(t, err)
	assert.Equal(t, openCodeInstallGitignore, string(raw))
	assert.NoFileExists(t, filepath.Join(privateConfig, openCodeInstallBootstrapFile),
		"seeding the read-only target itself would not be visible inside the sandbox")
}

func TestPrepareOpenCodeReadOnlyConfigRefusesUnsafeBootstrap(t *testing.T) {
	for _, platform := range []string{"Linux", "Darwin"} {
		t.Run(platform, func(t *testing.T) {
			root := t.TempDir()
			configDir := filepath.Join(t.TempDir(), "opencode")
			require.NoError(t, os.Mkdir(configDir, 0o700))
			require.NoError(t, os.WriteFile(
				filepath.Join(configDir, "target"), []byte("x"), 0o600))
			require.NoError(t, os.Symlink("target",
				filepath.Join(configDir, openCodeInstallBootstrapFile)))

			err := prepareOpenCodeReadOnlyConfig(
				openCodeConfigBootstrapSpec(root, configDir, configDir), platform)
			require.ErrorContains(t, err, "opencode_read_only_config_bootstrap")
			require.ErrorContains(t, err, platform)
			require.ErrorContains(t, err, "existing OpenCode config bootstrap")
		})
	}
}

// A config app directory served by no read-only bind is writable in the
// sandbox, so nothing needs to be planted in the operator's tree for it.
func TestPrepareOpenCodeReadOnlyConfigSkipsWritableConfig(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(t.TempDir(), "opencode")
	require.NoError(t, os.Mkdir(configDir, 0o700))
	spec := openCodeConfigBootstrapSpec(root, configDir, configDir)
	spec.Contract.ReadOnlyBinds = nil

	require.NoError(t, prepareOpenCodeReadOnlyConfig(spec, "Linux"))
	assert.NoFileExists(t, filepath.Join(configDir, openCodeInstallBootstrapFile))
}

// Falsifiability anchor for TCL-892: the host's own hook must reach the shared
// implementation. With the Linux hook back to its pre-TCL-892 `return nil`,
// this fails on Linux.
func TestPrepareOpenCodeReadOnlyConfigForPlatformIsWired(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(t.TempDir(), "opencode")
	require.NoError(t, os.Mkdir(configDir, 0o700))

	require.NoError(t, prepareOpenCodeReadOnlyConfigForPlatform(
		openCodeConfigBootstrapSpec(root, configDir, configDir)))

	raw, err := os.ReadFile(filepath.Join(configDir, openCodeInstallBootstrapFile))
	require.NoError(t, err)
	assert.Equal(t, openCodeInstallGitignore, string(raw))
}

func openCodeConfigBootstrapSpec(
	root, configDir, bindSource string,
) *session.TclaudeLayerLaunchSpec {
	return &session.TclaudeLayerLaunchSpec{
		Contract: session.TclaudeLayerLaunchContract{
			HarnessName: harness.OpenCodeName,
			StateRoot:   root,
			StateDirs: []string{
				filepath.Join(root, "data", "opencode"),
				filepath.Join(root, "cache", "opencode"),
				configDir,
				filepath.Join(root, "state", "opencode"),
			},
			ReadOnlyBinds: []session.TclaudeLayerReadOnlyBind{{
				Source: bindSource,
				Target: configDir,
			}},
		},
	}
}

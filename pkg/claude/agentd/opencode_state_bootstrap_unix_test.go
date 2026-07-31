//go:build linux || darwin

package agentd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
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
	configBase := filepath.Join(t.TempDir(), "config")
	ambientConfig := filepath.Join(configBase, "opencode")
	require.NoError(t, os.MkdirAll(ambientConfig, 0o700))
	t.Setenv("XDG_CONFIG_HOME", configBase)
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

// A bind source is not covered by the launch contract's own validation, which
// checks bind targets, so a replayed or tampered spec must not be able to aim a
// daemon-side write at an arbitrary directory.
func TestPrepareOpenCodeReadOnlyConfigRefusesForeignBindSource(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(t.TempDir(), "opencode")
	require.NoError(t, os.Mkdir(configDir, 0o700))
	foreign := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "config"))

	err := prepareOpenCodeReadOnlyConfig(
		openCodeConfigBootstrapSpec(root, configDir, foreign), "Linux")
	require.ErrorContains(t, err, "opencode_read_only_config_bootstrap")
	require.ErrorContains(t, err, "neither the contract's config directory")
	assert.NoFileExists(t, filepath.Join(foreign, openCodeInstallBootstrapFile))
}

// When more than one read-only bind names the config directory, the one the
// sandbox serves is the LAST. Seeding an earlier source would write a file
// nothing inside the sandbox can see.
func TestPrepareOpenCodeReadOnlyConfigSeedsTheServingBind(t *testing.T) {
	root := t.TempDir()
	configBase := filepath.Join(t.TempDir(), "config")
	ambientConfig := filepath.Join(configBase, "opencode")
	require.NoError(t, os.MkdirAll(ambientConfig, 0o700))
	t.Setenv("XDG_CONFIG_HOME", configBase)
	privateConfig := filepath.Join(root, "config", "opencode")
	require.NoError(t, os.MkdirAll(privateConfig, 0o700))

	spec := openCodeConfigBootstrapSpec(root, privateConfig, ambientConfig)
	spec.Contract.ReadOnlyBinds = append(spec.Contract.ReadOnlyBinds,
		session.TclaudeLayerReadOnlyBind{
			Source: privateConfig, Target: privateConfig,
		})

	require.NoError(t, prepareOpenCodeReadOnlyConfig(spec, "Linux"))
	assert.FileExists(t, filepath.Join(privateConfig, openCodeInstallBootstrapFile))
	assert.NoFileExists(t,
		filepath.Join(ambientConfig, openCodeInstallBootstrapFile),
		"the losing bind's source is not what the sandbox reads")
}

// The unit cases above author their own contract, so nothing in them would
// notice if the production layout stopped emitting the bind shape the predicate
// looks for. This one builds the layout through the production path and feeds
// its own output to the predicate.
func TestPrepareOpenCodeReadOnlyConfigMatchesTheProducedLayout(t *testing.T) {
	home := t.TempDir()
	configBase := filepath.Join(home, "config")
	ambientConfig := filepath.Join(configBase, "opencode")
	require.NoError(t, os.MkdirAll(ambientConfig, 0o700))
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_CONFIG_HOME", configBase)
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))

	const agentID = "agt_0123456789abcdef0123456789abcdef"
	stateRoot := filepath.Join(home, "opencode-state", agentID)
	require.NoError(t, os.MkdirAll(stateRoot, 0o700))
	resolvedRoot, err := filepath.EvalSymlinks(stateRoot)
	require.NoError(t, err)

	layout, err := openCodeStateLayoutForAllocation(db.OpenCodeAgentStateAllocation{
		AgentID:   agentID,
		Mode:      db.OpenCodeStatePrivate,
		StateRoot: resolvedRoot,
	})
	require.NoError(t, err)
	require.Len(t, layout.stateDirs, 4)

	spec := &session.TclaudeLayerLaunchSpec{
		Contract: session.TclaudeLayerLaunchContract{
			HarnessName:   harness.OpenCodeName,
			StateRoot:     resolvedRoot,
			StateDirs:     layout.stateDirs,
			ReadOnlyBinds: layout.readOnlyBinds,
		},
	}
	require.NoError(t, prepareOpenCodeReadOnlyConfigForPlatform(spec))

	resolvedAmbient, err := filepath.EvalSymlinks(ambientConfig)
	require.NoError(t, err)
	raw, err := os.ReadFile(
		filepath.Join(resolvedAmbient, openCodeInstallBootstrapFile))
	require.NoError(t, err,
		"the layout's config bind serves the ambient directory, so that is where the bootstrap has to land")
	assert.Equal(t, openCodeInstallGitignore, string(raw))
}

// The bind source and the host's ambient config can name the same directory
// through different paths — macOS reaches its temp root through /var, a
// symlink to /private/var, which is how this first failed in CI. The source
// check compares directories, not strings.
func TestPrepareOpenCodeReadOnlyConfigAcceptsSymlinkedAmbientConfig(t *testing.T) {
	root := t.TempDir()
	real := t.TempDir()
	configBase := filepath.Join(real, "config")
	ambientConfig := filepath.Join(configBase, "opencode")
	require.NoError(t, os.MkdirAll(ambientConfig, 0o700))

	link := filepath.Join(t.TempDir(), "link")
	require.NoError(t, os.Symlink(real, link))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(link, "config"))

	privateConfig := filepath.Join(root, "config", "opencode")
	require.NoError(t, os.MkdirAll(privateConfig, 0o700))
	spec := openCodeConfigBootstrapSpec(root, privateConfig, ambientConfig)

	require.NoError(t, prepareOpenCodeReadOnlyConfig(spec, "Darwin"))
	assert.FileExists(t, filepath.Join(ambientConfig, openCodeInstallBootstrapFile))
}

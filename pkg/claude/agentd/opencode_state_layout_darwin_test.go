//go:build darwin

package agentd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func TestDarwinOpenCodeStateLayoutUsesCanonicalAmbientConfigWithoutProjection(t *testing.T) {
	root := "/Users/dev/state/agt_0123456789abcdef0123456789abcdef"
	privateConfig := filepath.Join(root, "config", "opencode")
	ambientConfig := "/Users/dev/.config/opencode"
	install := "/Users/dev/.opencode"
	layout := &openCodeStateLayout{
		allocation: db.OpenCodeAgentStateAllocation{
			AgentID: "agt_0123456789abcdef0123456789abcdef",
			Mode:    db.OpenCodeStatePrivate,
		},
		ambient: struct {
			data, cache, config, state, install string
		}{
			config: ambientConfig,
		},
		stateDirs: []string{
			filepath.Join(root, "data", "opencode"),
			filepath.Join(root, "cache", "opencode"),
			privateConfig,
			filepath.Join(root, "state", "opencode"),
		},
		environment: []sandboxpolicy.EnvironmentEntry{
			{Name: "XDG_DATA_HOME", Value: filepath.Join(root, "data")},
			{Name: "XDG_CACHE_HOME", Value: filepath.Join(root, "cache")},
			{Name: "XDG_CONFIG_HOME", Value: filepath.Join(root, "config")},
			{Name: "XDG_STATE_HOME", Value: filepath.Join(root, "state")},
		},
		readOnlyBinds: []session.TclaudeLayerReadOnlyBind{
			{Source: ambientConfig, Target: ambientConfig},
			{Source: ambientConfig, Target: privateConfig},
			{Source: install, Target: install},
		},
	}

	require.NoError(t, adaptOpenCodeStateLayoutForPlatform(layout))
	assert.Equal(t, filepath.Dir(ambientConfig), layout.environment[2].Value)
	assert.Equal(t, ambientConfig, layout.stateDirs[2])
	assert.Equal(t, []session.TclaudeLayerReadOnlyBind{
		{Source: ambientConfig, Target: ambientConfig},
		{Source: install, Target: install},
	}, layout.readOnlyBinds)
	for _, bind := range layout.readOnlyBinds {
		assert.Equal(t, bind.Source, bind.Target,
			"fresh Darwin contracts must never ask Seatbelt for path projection")
	}
}

func TestDarwinOpenCodeStateLayoutKeepsEmptyPrivateConfig(t *testing.T) {
	root := "/Users/dev/state/agt_0123456789abcdef0123456789abcdef"
	privateConfig := filepath.Join(root, "config", "opencode")
	layout := &openCodeStateLayout{
		allocation: db.OpenCodeAgentStateAllocation{
			AgentID: "agt_0123456789abcdef0123456789abcdef",
			Mode:    db.OpenCodeStatePrivate,
		},
		ambient: struct {
			data, cache, config, state, install string
		}{
			config: filepath.Join(root, "config", "opencode"),
		},
		stateDirs: []string{
			filepath.Join(root, "data", "opencode"),
			filepath.Join(root, "cache", "opencode"),
			privateConfig,
			filepath.Join(root, "state", "opencode"),
		},
		environment: []sandboxpolicy.EnvironmentEntry{
			{Name: "XDG_DATA_HOME", Value: filepath.Join(root, "data")},
			{Name: "XDG_CACHE_HOME", Value: filepath.Join(root, "cache")},
			{Name: "XDG_CONFIG_HOME", Value: filepath.Join(root, "config")},
			{Name: "XDG_STATE_HOME", Value: filepath.Join(root, "state")},
		},
		readOnlyBinds: []session.TclaudeLayerReadOnlyBind{{
			Source: privateConfig,
			Target: privateConfig,
		}},
	}

	require.NoError(t, adaptOpenCodeStateLayoutForPlatform(layout))
	assert.Equal(t, filepath.Join(root, "config"), layout.environment[2].Value)
	assert.Equal(t, privateConfig, layout.stateDirs[2])
	assert.Equal(t, layout.readOnlyBinds[0].Source, layout.readOnlyBinds[0].Target)
}

func TestDarwinOpenCodeStateLayoutPreservesConfigBaseAcrossLeafSymlink(t *testing.T) {
	root := "/Users/dev/state/agt_0123456789abcdef0123456789abcdef"
	privateConfig := filepath.Join(root, "config", "opencode")
	ambientConfig := "/Users/dev/.config/opencode"
	resolvedConfig := "/Volumes/dotfiles/opencode-global"
	layout := &openCodeStateLayout{
		allocation: db.OpenCodeAgentStateAllocation{
			AgentID: "agt_0123456789abcdef0123456789abcdef",
			Mode:    db.OpenCodeStatePrivate,
		},
		ambient: struct {
			data, cache, config, state, install string
		}{
			config: ambientConfig,
		},
		stateDirs: []string{
			filepath.Join(root, "data", "opencode"),
			filepath.Join(root, "cache", "opencode"),
			privateConfig,
			filepath.Join(root, "state", "opencode"),
		},
		environment: []sandboxpolicy.EnvironmentEntry{
			{Name: "XDG_DATA_HOME", Value: filepath.Join(root, "data")},
			{Name: "XDG_CACHE_HOME", Value: filepath.Join(root, "cache")},
			{Name: "XDG_CONFIG_HOME", Value: filepath.Join(root, "config")},
			{Name: "XDG_STATE_HOME", Value: filepath.Join(root, "state")},
		},
		readOnlyBinds: []session.TclaudeLayerReadOnlyBind{
			{Source: resolvedConfig, Target: resolvedConfig},
			{Source: resolvedConfig, Target: privateConfig},
		},
	}

	require.NoError(t, adaptOpenCodeStateLayoutForPlatform(layout))
	assert.Equal(t, filepath.Dir(ambientConfig), layout.environment[2].Value,
		"non-OpenCode config writes must keep targeting the real XDG base")
	assert.Equal(t, resolvedConfig, layout.stateDirs[2],
		"OpenCode's app directory must retain its resolved filesystem identity")
}

func TestPrepareDarwinOpenCodeReadOnlyConfigBootstrapsWithoutOverwrite(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(t.TempDir(), "opencode")
	require.NoError(t, os.Mkdir(configDir, 0o700))
	spec := darwinOpenCodeConfigBootstrapSpec(root, configDir)

	require.NoError(t, prepareOpenCodeReadOnlyConfigForPlatform(spec))
	path := filepath.Join(configDir, openCodeInstallBootstrapFile)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, openCodeInstallGitignore, string(raw))

	require.NoError(t, os.WriteFile(path, []byte("operator-owned"), 0o640))
	require.NoError(t, prepareOpenCodeReadOnlyConfigForPlatform(spec))
	raw, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "operator-owned", string(raw),
		"the pre-wall bootstrap must never overwrite existing host config metadata")
}

func TestPrepareDarwinOpenCodeReadOnlyConfigRefusesUnsafeBootstrap(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(t.TempDir(), "opencode")
	require.NoError(t, os.Mkdir(configDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(configDir, "target"), []byte("x"), 0o600))
	require.NoError(t, os.Symlink("target",
		filepath.Join(configDir, openCodeInstallBootstrapFile)))

	err := prepareOpenCodeReadOnlyConfigForPlatform(
		darwinOpenCodeConfigBootstrapSpec(root, configDir))
	require.ErrorContains(t, err, "opencode_read_only_config_bootstrap")
	require.ErrorContains(t, err, "existing OpenCode config bootstrap")
}

func darwinOpenCodeConfigBootstrapSpec(
	root, configDir string,
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
				Source: configDir,
				Target: configDir,
			}},
		},
	}
}

//go:build darwin

package agentd

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
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

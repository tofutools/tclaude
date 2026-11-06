package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestOpenCodeSelectedModelMayLeaveProviderRouteUnknown(t *testing.T) {
	_, cwd := isolateModelTransportLaunch(t)
	openCode := harness.MustGet(harness.OpenCodeName)

	resolved, err := ResolveTclaudeLayerModelTransport(openCode,
		ModelTransportLaunchContext{Model: "sonnet", Cwd: cwd})
	require.NoError(t, err)
	assert.Equal(t, "sonnet", resolved.Model)
	assert.False(t, resolved.ProviderResolved)

	resolved, err = ResolveTclaudeLayerModelTransport(openCode,
		ModelTransportLaunchContext{Model: "corp/model", Cwd: cwd})
	require.NoError(t, err)
	assert.Equal(t, "corp/model", resolved.Model)
	assert.False(t, resolved.ProviderResolved)
}

// An unresolved OpenCode route under a filtered network still launches, but
// the launch must say that tclaude did not check the model endpoint and where
// the fix lives. The notice adds no access.
func TestOpenCodeUnresolvedRouteUnderFilteredNetworkIsDisclosed(t *testing.T) {
	_, cwd := isolateModelTransportLaunch(t)
	openCode := harness.MustGet(harness.OpenCodeName)
	resolved, err := ResolveTclaudeLayerModelTransport(openCode,
		ModelTransportLaunchContext{Model: "corp/model", Cwd: cwd})
	require.NoError(t, err)
	require.False(t, resolved.ProviderResolved)
	assert.Contains(t, resolved.UnresolvedReason, "OPENCODE_CONFIG_CONTENT")

	effective := sandboxpolicy.EffectiveProfile{Network: &sandboxpolicy.NetworkRules{
		Mode:  sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{{Host: "git.example", Ports: []int{443}}},
	}}
	notices, err := ValidateTclaudeLayerNetwork(openCode, effective, resolved)
	require.NoError(t, err, "an unknown OpenCode route is not grounds to refuse")
	require.Len(t, notices, 1)
	assert.Equal(t, sandboxpolicy.AccessNoticeReasonFilteredModelTraffic, notices[0].Reason)
	assert.Contains(t, notices[0].Detail, "could not resolve this OpenCode model's provider endpoint")
	assert.Contains(t, notices[0].Detail, "allow the provider's API host in every layer")
	assert.Contains(t, notices[0].Detail, "Resolution stopped at:")
	assert.Equal(t, []sandboxpolicy.NetworkAllowEntry{{Host: "git.example", Ports: []int{443}}},
		effective.Network.Allow, "the notice must not add model access")
}

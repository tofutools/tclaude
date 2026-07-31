package session

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// TCL-895, launch half. The preset refusal is the packet gateway's — it exists
// because that gateway admits a destination only against a launch endpoint
// resolved ahead of time — so it is gated on the deployed engine inside the
// validator rather than at its two call sites (the session boundary and the
// daemon spawn guard), which is what keeps them from drifting apart or from the
// rendered row.
func TestOpenCodeLocalPresetLaunchGateIsEngineGated(t *testing.T) {
	openCode := harness.MustGet(harness.OpenCodeName)
	preset := sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{
			{Domain: "api.anthropic.com", Ports: []int{443}},
			{Domain: "api.openai.com", Ports: []int{443}},
			{Loopback: true},
		},
	}
	require.True(t, harness.IsLocalModelAPIsNetworkPreset(preset))

	packetRules := preset
	packetEffective := sandboxpolicy.EffectiveProfile{Network: &packetRules}
	engine, err := TclaudeLayerNetworkEngine(packetEffective)
	require.NoError(t, err)
	require.Equal(t, sandboxpolicy.NetworkEnginePacket, engine)
	err = ValidateTclaudeLayerOpenCodeLocalModelTransport(
		openCode, packetEffective, ModelTransportLaunchContext{Model: "corp/model"})
	require.Error(t, err, "the packet gateway's refusal is unchanged")
	require.ErrorContains(t, err, "no explicit provider")

	proxyRules := preset
	proxyRules.Engine = sandboxpolicy.NetworkEngineProxy
	proxyEffective := sandboxpolicy.EffectiveProfile{Network: &proxyRules}
	engine, err = TclaudeLayerNetworkEngine(proxyEffective)
	require.NoError(t, err)
	require.Equal(t, sandboxpolicy.NetworkEngineProxy, engine,
		"the preset must actually deploy the proxy, or this case proves nothing")

	proxyErr := ValidateTclaudeLayerOpenCodeLocalModelTransport(
		openCode, proxyEffective, ModelTransportLaunchContext{Model: "corp/model"})
	// Deploying the proxy is not enough on its own: the relaxation also needs
	// this harness's proxy cells to be ACTIVATED on this platform, because a
	// platform whose cells enforce nothing would have the policy widened to
	// open instead of refused. The expectation is therefore READ FROM the
	// predicate rather than hard-coded, so this test says the same thing on a
	// Linux runner and a macOS one.
	if harness.ProxyEngineActivated(openCode.Name, runtime.GOOS) {
		require.NoError(t, proxyErr,
			"a proxy-engine launch runs none of the machinery this refusal describes")
	} else {
		require.Error(t, proxyErr,
			"an unactivated platform must keep the refusal rather than widen to open")
		require.ErrorContains(t, proxyErr, "no explicit provider")
	}
}

// The gate above is not a hole in the OpenCode launch contract: the general
// model-transport resolve is ENGINE-INDEPENDENT and still refuses a launch
// without an explicit provider/model and inline explicit-provider config —
// which is exactly what the activated proxy row's
// OpenCodeFilteredExplicitProviderCaveat discloses.
func TestOpenCodeProxyEngineStillRequiresAnExplicitProvider(t *testing.T) {
	_, cwd := isolateModelTransportLaunch(t)
	openCode := harness.MustGet(harness.OpenCodeName)

	_, err := ResolveTclaudeLayerModelTransport(openCode,
		ModelTransportLaunchContext{Model: "sonnet", Cwd: cwd})
	require.Error(t, err)
	assert.Contains(t, err.Error(),
		"requires an explicit provider/model launch model")

	_, err = ResolveTclaudeLayerModelTransport(openCode,
		ModelTransportLaunchContext{Model: "corp/model", Cwd: cwd})
	require.Error(t, err,
		"an explicit provider/model still needs the inline explicit-provider config")
}

// A loopback-ONLY Local-access preset is not discriminating, so it deploys no
// engine at all — the floor expresses loopback natively — and an authored
// engine has nothing to take effect on. Recorded as an assertion because it is
// the shape the ticket named, and because a future reader who tries the ticket's
// literal example and sees no change deserves to find the reason here.
func TestLoopbackOnlyLocalAccessPresetDeploysNoEngine(t *testing.T) {
	rules := sandboxpolicy.NetworkRules{
		Mode:   sandboxpolicy.AccessModeList,
		Allow:  []sandboxpolicy.NetworkAllowEntry{{Loopback: true}},
		Engine: sandboxpolicy.NetworkEngineProxy,
	}
	require.True(t, harness.IsLocalAccessNetworkPreset(rules))
	effective := sandboxpolicy.EffectiveProfile{Network: &rules}
	engine, err := TclaudeLayerNetworkEngine(effective)
	require.NoError(t, err)
	assert.Equal(t, sandboxpolicy.NetworkEngineUnset, engine)
	require.Error(t, ValidateTclaudeLayerOpenCodeLocalModelTransport(
		harness.MustGet(harness.OpenCodeName), effective,
		ModelTransportLaunchContext{Model: "corp/model"}),
		"no proxy is deployed here, so the packet-shaped refusal still holds")
}

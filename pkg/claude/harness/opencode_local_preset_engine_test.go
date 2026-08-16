package harness

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/sandboxproxy"
)

// Local presets describe enforceable network rules independently of whether a
// launch model was selected. Both engines therefore advertise and plan their
// actual network enforcement; provider coverage is checked later only for an
// explicitly selected model.
func TestOpenCodeLocalPresetsDoNotRequireProviderSelection(t *testing.T) {
	for _, preset := range []struct {
		name  string
		rules sandboxpolicy.NetworkRules
	}{
		{
			// The Local-access preset carries a deny row, and it has to: a
			// loopback-ONLY allow list is not discriminating, so it deploys no
			// engine at all (the floor expresses loopback natively) and no
			// engine gate can change what it renders. A deny row is what makes
			// the same preset discriminating, and therefore what makes an
			// authored engine take effect on it.
			name: "local access with a deny row",
			rules: sandboxpolicy.NetworkRules{
				Mode:  sandboxpolicy.AccessModeList,
				Allow: []sandboxpolicy.NetworkAllowEntry{{Loopback: true}},
				Deny: []sandboxpolicy.NetworkAllowEntry{
					{Domain: "evil.example.com", Ports: []int{443}},
				},
			},
		},
		{
			name: "local model APIs",
			rules: sandboxpolicy.NetworkRules{
				Mode: sandboxpolicy.AccessModeList,
				Allow: []sandboxpolicy.NetworkAllowEntry{
					{Domain: "api.anthropic.com", Ports: []int{443}},
					{Domain: "api.openai.com", Ports: []int{443}},
					{Loopback: true},
				},
			},
		},
	} {
		t.Run(preset.name, func(t *testing.T) {
			require.True(t,
				IsLocalAccessNetworkPreset(preset.rules) ||
					IsLocalModelAPIsNetworkPreset(preset.rules),
				"this case must actually be one of the two presets the override names")

			// Packet engine enforces the authored preset without inventing a
			// provider requirement.
			packet, err := PredictAccessEnforcement(
				MustGet(OpenCodeName), sandboxpolicy.ImplementationTclaudeLayer,
				sandboxpolicy.ResolvedAxes{Network: preset.rules}, "", "linux",
			)
			require.NoError(t, err)
			assert.Equal(t, EnforceFull, packet.NetworkList)
			assert.Empty(t, packet.NetworkListRefusal)

			// Proxy engine: the activated proxy cells, no packet refusal.
			proxyRules := preset.rules
			proxyRules.Engine = sandboxpolicy.NetworkEngineProxy
			engine, err := sandboxpolicy.DeployedNetworkEngineForRules(proxyRules)
			require.NoError(t, err)
			require.Equal(t, sandboxpolicy.NetworkEngineProxy, engine,
				"the preset must actually deploy the proxy, or this case proves nothing")

			proxy, err := PredictAccessEnforcement(
				MustGet(OpenCodeName), sandboxpolicy.ImplementationTclaudeLayer,
				sandboxpolicy.ResolvedAxes{Network: proxyRules}, "", "linux",
			)
			require.NoError(t, err)
			assert.Empty(t, proxy.NetworkListRefusal,
				"a proxy-engine launch runs none of the machinery this refusal describes")
			assert.Equal(t, EnforceFull, proxy.NetworkList)
			assert.Equal(t, EnforceFull, proxy.NetworkPorts)
			require.NotEmpty(t, proxy.NetworkSelectors)
			// The condition explains the selected-model check and the no-model
			// user-managed behavior without making it an unconditional gate.
			assert.Contains(t, proxy.NetworkListCondition,
				OpenCodeFilteredExplicitProviderCaveat)

			// The rendered allow rows are what the REAL evaluator enforces —
			// the same one the running proxy compiles. Rating these cells Full
			// without checking that would be a claim about the engine rather
			// than an observation of it.
			evaluator, err := sandboxproxy.NewEvaluator(proxyRules)
			require.NoError(t, err)
			loopback, err := sandboxproxy.ParseTarget("127.0.0.1", 11434)
			require.NoError(t, err)
			assert.True(t, evaluator.Evaluate(loopback).Allowed(),
				"the preset authorizes loopback, so the proxy must admit it")
			outside, err := sandboxproxy.ParseTarget("evil.example.com", 443)
			require.NoError(t, err)
			assert.False(t, evaluator.Evaluate(outside).Allowed(),
				"a destination outside the preset must be refused, or Full is an over-claim")

			// And the preset composes into a plan at all, which is the
			// user-visible symptom: both engine shapes compose successfully.
			_, _, err = PlanAccessEnforcement(
				sandboxpolicy.ResolvedAxes{Network: proxyRules},
				accessEnforcementFromTable(mustAccessEnforcementTable(
					t, proxyRules)))
			require.NoError(t, err)
			_, _, err = PlanAccessEnforcement(
				sandboxpolicy.ResolvedAxes{Network: preset.rules},
				accessEnforcementFromTable(mustAccessEnforcementTable(
					t, preset.rules)))
			require.NoError(t, err)
		})
	}
}

func TestOtherHarnessesAlsoEnforceLocalPresets(t *testing.T) {
	rules := sandboxpolicy.NetworkRules{
		Mode:  sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{{Loopback: true}},
	}
	for _, name := range []string{DefaultName, CodexName} {
		predicted, err := PredictAccessEnforcement(
			MustGet(name), sandboxpolicy.ImplementationTclaudeLayer,
			sandboxpolicy.ResolvedAxes{Network: rules}, "", "linux",
		)
		require.NoErrorf(t, err, "harness %s", name)
		assert.Emptyf(t, predicted.NetworkListRefusal,
			"harness %s should enforce the local preset", name)
	}
}

func mustAccessEnforcementTable(
	t *testing.T,
	rules sandboxpolicy.NetworkRules,
) accessEnforcementTableRow {
	t.Helper()
	row, err := accessEnforcementTable(
		MustGet(OpenCodeName), sandboxpolicy.ImplementationTclaudeLayer,
		sandboxpolicy.ResolvedAxes{Network: rules},
		OpenCodeSandboxTclaudeLayer, "linux", true,
	)
	require.NoError(t, err)
	return row
}

// An unactivated platform still refuses because it cannot enforce the network
// posture, not because OpenCode lacks a selected provider.
func TestOpenCodeLocalPresetKeepsNetworkRefusalWhereProxyCellsAreNotActivated(t *testing.T) {
	rules := sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{
			{Domain: "api.anthropic.com", Ports: []int{443}},
			{Domain: "api.openai.com", Ports: []int{443}},
			{Loopback: true},
		},
		Engine: sandboxpolicy.NetworkEngineProxy,
	}
	engine, err := sandboxpolicy.DeployedNetworkEngineForRules(rules)
	require.NoError(t, err)
	require.Equal(t, sandboxpolicy.NetworkEngineProxy, engine)
	const unactivatedPlatform = "freebsd"
	require.False(t, ProxyEngineActivated(OpenCodeName, unactivatedPlatform),
		"this case is about an unactivated platform")

	axes := sandboxpolicy.ResolvedAxes{Network: rules}
	predicted, err := PredictAccessEnforcement(
		MustGet(OpenCodeName), sandboxpolicy.ImplementationTclaudeLayer,
		axes, "", unactivatedPlatform,
	)
	require.NoError(t, err)
	assert.Equal(t, EnforceNone, predicted.NetworkList)
	assert.Contains(t, predicted.NetworkListRefusal, "unsupported_filtered_network_posture")
	assert.NotContains(t, predicted.NetworkListRefusal, SandboxCapabilityModelTransport)

	// And the plan refuses rather than widening. Without the activation
	// condition this call returns a widened, open policy with a notice.
	row, err := accessEnforcementTable(
		MustGet(OpenCodeName), sandboxpolicy.ImplementationTclaudeLayer,
		axes, OpenCodeSandboxTclaudeLayer, unactivatedPlatform, true,
	)
	require.NoError(t, err)
	rendered, _, err := PlanAccessEnforcement(axes, accessEnforcementFromTable(row))
	require.ErrorContains(t, err, "unsupported_filtered_network_posture")
	assert.NotEqual(t, sandboxpolicy.AccessModeOpen, rendered.Network.Mode,
		"a refused local preset must never reach a launch widened to open")
}

package harness

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/sandboxproxy"
)

// TCL-895. The OpenCode local-preset override zeroes the network cells and
// renders the packet gateway's model-transport refusal. That refusal exists
// because the gateway admits a destination only against a launch endpoint
// resolved AHEAD of time, and these presets give nothing to resolve one from.
// A proxy-engine launch resolves nothing ahead of time — it decides on the
// identity the client states at connect time — so the override is gated on the
// deployed engine, from the same derivation every other packet rating uses.
//
// Both presets are `mode: list` by definition, so the baseline axis these
// tests vary is the one the defect is about: the DEPLOYED ENGINE. Each case
// asserts the packet rendering is untouched and the proxy rendering changed,
// so a revert of the gate fails the proxy half rather than silently passing.
func TestOpenCodeLocalPresetOverrideIsGatedOnTheDeployedEngine(t *testing.T) {
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

			// Packet engine (nothing authored): unchanged, still refused.
			packet, err := PredictAccessEnforcement(
				MustGet(OpenCodeName), sandboxpolicy.ImplementationTclaudeLayer,
				sandboxpolicy.ResolvedAxes{Network: preset.rules}, "", "linux",
			)
			require.NoError(t, err)
			assert.Equal(t, EnforceNone, packet.NetworkList,
				"the packet gateway's refusal for these presets is not what this change touches")
			assert.Contains(t, packet.NetworkListRefusal,
				SandboxCapabilityModelTransport)

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
			// The launch gate is engine-INDEPENDENT and still applies, so the
			// activated row has to keep saying so: raising these cells without
			// the caveat would advertise enforcement for a launch that is then
			// refused for want of an explicit provider.
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
			// user-visible symptom: the packet shape below cannot.
			_, _, err = PlanAccessEnforcement(
				sandboxpolicy.ResolvedAxes{Network: proxyRules},
				accessEnforcementFromTable(mustAccessEnforcementTable(
					t, proxyRules)))
			require.NoError(t, err)
			_, _, err = PlanAccessEnforcement(
				sandboxpolicy.ResolvedAxes{Network: preset.rules},
				accessEnforcementFromTable(mustAccessEnforcementTable(
					t, preset.rules)))
			require.ErrorContains(t, err, SandboxCapabilityModelTransport)
		})
	}
}

// The override is OpenCode's alone: no other harness has a preset-shaped
// capability override to gate, so TCL-895's "check the other two while in
// there" is recorded as an assertion rather than as prose in a PR body.
func TestLocalPresetOverrideAppliesOnlyToOpenCode(t *testing.T) {
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
			"harness %s has no local-preset override to gate", name)
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

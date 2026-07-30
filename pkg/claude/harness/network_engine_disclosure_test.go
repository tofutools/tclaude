package harness

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func proxyEngineRules(allow ...sandboxpolicy.NetworkAllowEntry) sandboxpolicy.NetworkRules {
	return sandboxpolicy.NetworkRules{
		Mode:   sandboxpolicy.AccessModeList,
		Allow:  allow,
		Engine: sandboxpolicy.NetworkEngineProxy,
	}
}

// TestProxyEngineDoesNotClaimThePacketGatewaysCells is the honesty gate for the
// milestone. The proxy engine's ratings are unactivated, so a policy that
// deploys a proxy must not inherit the ratings of the pasta/nft gateway it does
// not run — and must not inherit its launch-check disclosure either.
func TestProxyEngineDoesNotClaimThePacketGatewaysCells(t *testing.T) {
	rules := proxyEngineRules(
		sandboxpolicy.NetworkAllowEntry{Domain: "example.com", Ports: []int{443}},
	)
	predicted, err := PredictAccessEnforcement(
		Default(), sandboxpolicy.ImplementationTclaudeLayer,
		sandboxpolicy.ResolvedAxes{Network: rules}, "", "linux",
	)
	require.NoError(t, err)
	assert.Equal(t, sandboxpolicy.NetworkEngineProxy, predicted.NetworkEngine)
	assert.Equal(t, EnforceNone, predicted.NetworkList,
		"proxy cells stay unenforced until their carriage smokes land")
	assert.Empty(t, predicted.NetworkSelectors)
	assert.Empty(t, predicted.NetworkDenySelectors)
	assert.Equal(t, ProxyEngineLinuxMechanism, predicted.Mechanism)
	assert.NotContains(t, predicted.NetworkListCondition, "pasta")

	// The same policy with no engine authored keeps every packet-gateway
	// rating, which is the parity half: unset changes nothing.
	rules.Engine = sandboxpolicy.NetworkEngineUnset
	unset, err := PredictAccessEnforcement(
		Default(), sandboxpolicy.ImplementationTclaudeLayer,
		sandboxpolicy.ResolvedAxes{Network: rules}, "", "linux",
	)
	require.NoError(t, err)
	assert.Equal(t, sandboxpolicy.NetworkEnginePacket, unset.NetworkEngine,
		"an unset selection under a discriminating policy is still the packet gateway")
	assert.Equal(t, EnforceFull, unset.NetworkList)
	assert.NotEmpty(t, unset.NetworkSelectors)
	assert.Empty(t, unset.NetworkEngineDetail,
		"an unset engine must add no sentence to the rendered surface")
}

// TestProxyEngineWholePostureDisclosure covers §5.3's whole-posture notice and
// §1.3-4's latent selection, both of which are read from the network axis.
func TestProxyEngineWholePostureDisclosure(t *testing.T) {
	deployed := networkEngineDisclosure(
		sandboxpolicy.NetworkEngineProxy, sandboxpolicy.NetworkEngineProxy)
	assert.Contains(t, deployed, "Filtering engine: Proxy filter.")
	assert.Contains(t, deployed, ProxyEngineCarriageNotice)
	assert.Contains(t, deployed, ProxyEngineNotActivatedNotice)
	assert.NotContains(t, deployed, ProxyEngineLatentSelectionNotice)

	latent := networkEngineDisclosure(
		sandboxpolicy.NetworkEngineProxy, sandboxpolicy.NetworkEngineUnset)
	assert.Contains(t, latent, ProxyEngineLatentSelectionNotice)
	assert.NotContains(t, latent, ProxyEngineCarriageNotice,
		"a policy that deploys no proxy must not describe what a proxy carries")

	assert.Empty(t, networkEngineDisclosure(
		sandboxpolicy.NetworkEngineUnset, sandboxpolicy.NetworkEnginePacket),
		"an unset selection renders nothing")
}

// TestProxyEngineMarksNonHTTPEntriesPartial covers §5.3's per-entry outcome.
// The rows that earn it are the ones a proxy-unaware client is most likely to
// open a socket to, which under this engine is blocked rather than filtered.
//
// The capability row is constructed with the proxy engine ALREADY activated,
// because that is the state the per-entry rule exists for; while the cells stay
// EnforceNone every row reports not-enforced instead, and the disclosure this
// test pins is what appears the moment M2.4 flips them.
func TestProxyEngineMarksNonHTTPEntriesPartial(t *testing.T) {
	caps := PredictedAccessEnforcement{
		NetworkList: EnforceFull,
		NetworkSelectors: []NetworkSelectorCapability{
			{Selector: string(sandboxpolicy.NetworkSelectorCIDR), Level: EnforceFull},
			{Selector: string(sandboxpolicy.NetworkSelectorDomain), Level: EnforceFull},
		},
		NetworkPorts:  EnforceFull,
		Scope:         "process",
		Mechanism:     ProxyEngineLinuxMechanism,
		NetworkEngine: sandboxpolicy.NetworkEngineProxy,
	}
	rows := DescribePredictedNetworkEntries(proxyEngineRules(
		sandboxpolicy.NetworkAllowEntry{Domain: "example.com", Ports: []int{443}},
		sandboxpolicy.NetworkAllowEntry{CIDR: "10.20.0.0/16", Ports: []int{5432}},
	), caps)
	require.Len(t, rows, 2)

	assert.Equal(t, AccessPredictionEnforced, rows[0].Outcome,
		"an HTTPS destination is carried by the proxy without a caveat")
	assert.NotContains(t, rows[0].Detail, ProxyEngineEntryCarriageDetail)

	assert.Equal(t, AccessPredictionEnforcedPartial, rows[1].Outcome)
	assert.Contains(t, rows[1].Detail, ProxyEngineEntryCarriageDetail)

	// Under the packet engine the same rows carry no proxy caveat: the caveat
	// is a property of the carriage, not of the port.
	caps.NetworkEngine = sandboxpolicy.NetworkEnginePacket
	packetRows := DescribePredictedNetworkEntries(proxyEngineRules(
		sandboxpolicy.NetworkAllowEntry{CIDR: "10.20.0.0/16", Ports: []int{5432}},
	), caps)
	require.Len(t, packetRows, 1)
	assert.Equal(t, AccessPredictionEnforced, packetRows[0].Outcome)
}

// TestProxyEngineRefusesAnAuthoredResolverSocket is the carried security item.
// An authored resolver socket restores in-sandbox name-to-literal conversion,
// which leaves the engine's host and domain rules with no name to decide on.
func TestProxyEngineRefusesAnAuthoredResolverSocket(t *testing.T) {
	axes := sandboxpolicy.ResolvedAxes{
		Network: proxyEngineRules(
			sandboxpolicy.NetworkAllowEntry{Domain: "example.com", Ports: []int{443}},
		),
		UnixSockets: sandboxpolicy.UnixSocketRules{
			Mode: sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.SocketAllowEntry{
				{Path: "/run/systemd/resolve/io.systemd.Resolve"},
			},
		},
	}
	predicted, err := PredictAccessEnforcement(
		Default(), sandboxpolicy.ImplementationTclaudeLayer, axes, "", "linux",
	)
	require.NoError(t, err)
	assert.Equal(t, EnforceNone, predicted.SocketList)
	assert.Contains(t, predicted.SocketListRefusal, "proxy_engine_name_authority")
	assert.Contains(t, predicted.SocketListRefusal,
		"/run/systemd/resolve/io.systemd.Resolve")
	assert.Contains(t, predicted.SocketListRefusal, "Packet filter",
		"a capability refusal must name its remedy")

	// The launch reaches the same verdict from the same row rather than from a
	// second copy of the rule.
	_, _, planErr := PlanAccessEnforcement(
		axes, accessEnforcementFromTable(mustEngineTableRow(t, axes)),
	)
	require.Error(t, planErr)
	assert.Contains(t, planErr.Error(), "proxy_engine_name_authority")

	// An ordinary socket under the same engine is untouched, and so is the same
	// resolver socket under the packet engine, whose DNS broker holds name
	// authority with a resolver present.
	ordinary := axes
	ordinary.UnixSockets = sandboxpolicy.UnixSocketRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.SocketAllowEntry{
			{Path: "/run/user/1000/podman/podman.sock"},
		},
	}
	predicted, err = PredictAccessEnforcement(
		Default(), sandboxpolicy.ImplementationTclaudeLayer, ordinary, "", "linux",
	)
	require.NoError(t, err)
	assert.Empty(t, predicted.SocketListRefusal)

	packet := axes
	packet.Network.Engine = sandboxpolicy.NetworkEnginePacket
	predicted, err = PredictAccessEnforcement(
		Default(), sandboxpolicy.ImplementationTclaudeLayer, packet, "", "linux",
	)
	require.NoError(t, err)
	assert.Empty(t, predicted.SocketListRefusal)
}

// TestProxyEngineRefusesAResolverReachingGlob keeps the refusal from being a
// spelling check: a bounded glob that would cover the socket authorizes it.
func TestProxyEngineRefusesAResolverReachingGlob(t *testing.T) {
	selector, resolver, found := sandboxpolicy.NetworkEngineResolverSocketConflict(
		sandboxpolicy.NetworkEngineProxy,
		sandboxpolicy.UnixSocketRules{
			Mode: sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.SocketAllowEntry{
				{PathGlob: "/run/systemd/resolve/*"},
			},
		},
	)
	require.True(t, found)
	assert.Equal(t, "/run/systemd/resolve/*", selector)
	assert.Contains(t, resolver, "io.systemd.Resolve")
}

func mustEngineTableRow(
	t *testing.T,
	axes sandboxpolicy.ResolvedAxes,
) accessEnforcementTableRow {
	t.Helper()
	row, err := accessEnforcementTable(
		Default(), sandboxpolicy.ImplementationTclaudeLayer, axes, "", "linux", true,
	)
	require.NoError(t, err)
	return row
}

// TestCapabilityLadderKeepsTheEngineThroughWidening is the launch-side twin of
// the planned-axes test. The ladder rewrites an unenforceable list into an open
// posture before the applier sees it; the engine has to survive that rewrite or
// the rendered policy the launch acts on names no mechanism at all.
func TestCapabilityLadderKeepsTheEngineThroughWidening(t *testing.T) {
	axes := sandboxpolicy.ResolvedAxes{
		Network: sandboxpolicy.NetworkRules{
			Mode:   sandboxpolicy.AccessModeList,
			Allow:  []sandboxpolicy.NetworkAllowEntry{{Host: "example.com"}},
			Engine: sandboxpolicy.NetworkEngineProxy,
		},
	}
	// A darwin tclaude-layer target cannot express this list, so the ladder
	// widens it rather than refusing.
	row, err := accessEnforcementTable(
		Default(), sandboxpolicy.ImplementationTclaudeLayer, axes, "", "darwin", false,
	)
	require.NoError(t, err)
	rendered, notices, err := PlanAccessEnforcement(
		axes, accessEnforcementFromTable(row))
	require.NoError(t, err)
	require.NotEmpty(t, notices)
	require.Equal(t, sandboxpolicy.AccessModeOpen, rendered.Network.Mode,
		"the list must have widened")
	assert.Equal(t, sandboxpolicy.NetworkEngineProxy, rendered.Network.Engine)
}

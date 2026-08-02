package harness

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/sandboxproxy"
)

func proxyEngineRules(allow ...sandboxpolicy.NetworkAllowEntry) sandboxpolicy.NetworkRules {
	return sandboxpolicy.NetworkRules{
		Mode:   sandboxpolicy.AccessModeList,
		Allow:  allow,
		Engine: sandboxpolicy.NetworkEngineProxy,
	}
}

// TestProxyEngineDoesNotClaimThePacketGatewaysCells is the honesty gate for the
// milestone, now on the other side of activation. An activated proxy engine
// must rate its OWN mechanism — never inherit the pasta/nft gateway's ratings,
// its DNS caveat, or its launch-check disclosure, none of which describe a
// launch that runs no pasta, no nft and no broker.
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
	assert.Equal(t, EnforceFull, predicted.NetworkList,
		"the proxy cells are activated by the named carriage smokes")
	assert.Equal(t, ProxyEngineLinuxMechanism, predicted.Mechanism)
	// The mechanism sentence, the launch condition and the name-selector detail
	// must all be the proxy's own. The condition is compared for EQUALITY rather
	// than for the absence of "pasta": the proxy's own condition mentions pasta
	// deliberately, to say it is not involved, and a substring check would both
	// fail on that and pass on a packet condition that happened to be reworded.
	assert.Equal(t, ProxyEngineLaunchCondition, predicted.NetworkListCondition)
	assert.NotContains(t, predicted.NetworkListCondition,
		"outbound traffic is open",
		"this floor refuses a launch it cannot build; it does not widen it")
	for _, capability := range predicted.NetworkSelectors {
		assert.NotContains(t, capability.Detail, "DNS lease",
			"the packet gateway's TTL caveat does not describe this engine")
	}

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
	unactivated := networkEngineDisclosure(
		DefaultName, sandboxpolicy.NetworkEngineProxy, sandboxpolicy.NetworkEngineProxy, false)
	assert.Contains(t, unactivated, "Filtering engine: Proxy filter.")
	assert.Contains(t, unactivated, ProxyEngineCarriageNotice)
	assert.Contains(t, unactivated, ProxyEngineNotActivatedNotice)
	assert.NotContains(t, unactivated, ProxyEngineLatentSelectionNotice)

	// Activated: the carriage notice STAYS — it describes what the engine
	// carries, which activation does not change — and only the not-yet-activated
	// sentence retires. A row whose cells say Full while the sentence beside
	// them says nothing is enforced is the contradiction this pins against.
	activated := networkEngineDisclosure(
		DefaultName, sandboxpolicy.NetworkEngineProxy, sandboxpolicy.NetworkEngineProxy, true)
	assert.Contains(t, activated, ProxyEngineCarriageNotice)
	assert.NotContains(t, activated, ProxyEngineNotActivatedNotice)

	latent := networkEngineDisclosure(
		DefaultName, sandboxpolicy.NetworkEngineProxy, sandboxpolicy.NetworkEngineUnset, false)
	assert.Contains(t, latent, ProxyEngineLatentSelectionNotice)
	assert.NotContains(t, latent, ProxyEngineCarriageNotice,
		"a policy that deploys no proxy must not describe what a proxy carries")

	// PER-HARNESS carriage, §5.3's second half. The engine's own carriage
	// sentence is the same for everyone; what differs is what THIS client uses
	// of it, and the two must not be smeared into one claim.
	//
	// Both directions are asserted, because only the pair is discriminating: a
	// sentence that appeared for every harness would say nothing about
	// OpenCode, and one that appeared for none would lose the measured fact.
	openCode := networkEngineDisclosure(
		OpenCodeName, sandboxpolicy.NetworkEngineProxy,
		sandboxpolicy.NetworkEngineProxy, true)
	assert.Contains(t, openCode, ProxyEngineCarriageNotice,
		"what the engine carries is unchanged by which client is on top of it")
	assert.Contains(t, openCode, ProxyEngineOpenCodeCarriageNotice)
	assert.NotContains(t, activated, ProxyEngineOpenCodeCarriageNotice,
		"a harness whose tool egress is measured over both carriages must not claim SOCKS is unreachable")

	assert.Empty(t, networkEngineDisclosure(
		DefaultName, sandboxpolicy.NetworkEngineUnset, sandboxpolicy.NetworkEnginePacket, false),
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

// TestProxyEngineRefusesAResolverReachingFilesystemGrant is TCL-883 at the
// capability seam. The socket axis was closed by TCL-882; a filesystem grant
// covering the resolver's directory binds the same inode into the constructed
// root and takes the same capability away, so it must refuse from the same
// evaluation — and from BOTH the preview and the launch, which is the property
// that keeps the editor from promising what a launch will refuse.
func TestProxyEngineRefusesAResolverReachingFilesystemGrant(t *testing.T) {
	axes := sandboxpolicy.ResolvedAxes{
		Network: proxyEngineRules(
			sandboxpolicy.NetworkAllowEntry{Domain: "example.com", Ports: []int{443}},
		),
		Filesystem: []sandboxpolicy.FilesystemGrant{
			{Path: "/srv/toolchain", Access: sandboxpolicy.AccessRead},
			{Path: "/run/systemd/resolve", Access: sandboxpolicy.AccessRead},
		},
	}

	_, predictErr := PredictAccessEnforcement(
		Default(), sandboxpolicy.ImplementationTclaudeLayer, axes, "", "linux",
	)
	require.Error(t, predictErr,
		"the editor preview must refuse rather than render a name authority it will not have")
	assert.Contains(t, predictErr.Error(), "proxy_engine_name_authority")
	assert.Contains(t, predictErr.Error(), "/run/systemd/resolve")
	assert.Contains(t, predictErr.Error(), "Packet filter engine",
		"a capability refusal must name its remedy")

	// The launch half reads runtime.GOOS rather than a parameter, and this
	// refusal is Linux-only by design: it describes a sandbox root built from
	// authored grants, which Seatbelt has no equivalent of. So the preview is
	// asked about Linux explicitly above, and the launch is asserted only where
	// a Linux launch is what would run.
	if runtime.GOOS == "linux" {
		_, resolveErr := ResolveAccessEnforcement(
			Default(), sandboxpolicy.ImplementationTclaudeLayer, axes,
			LaunchOSSandbox{State: "on", Source: "test bwrap", FilteredNetwork: true}, "",
		)
		require.Error(t, resolveErr,
			"the launch must refuse from the same evaluation the preview used")
		assert.Contains(t, resolveErr.Error(), "proxy_engine_name_authority")

		var capabilityErr *SandboxCapabilityError
		require.ErrorAs(t, resolveErr, &capabilityErr,
			"the refusal must be typed so callers attribute it to the network axis")
		assert.Equal(t, SandboxCapabilityNetworkAllowlist, capabilityErr.Kind)
	}

	// The Linux-only scope is a claim in its own right, so assert it rather
	// than only relying on the guard above: the identical axes on a darwin
	// target must NOT refuse, because no root is built from grants there.
	_, darwinErr := PredictAccessEnforcement(
		Default(), sandboxpolicy.ImplementationTclaudeLayer, axes, "", "darwin",
	)
	assert.NoError(t, darwinErr,
		"Seatbelt builds no root from grants; this refusal does not describe it")

	// A DENY-ONLY policy is the shape that made this a capability-seam refusal
	// rather than an allow-list cell: its network mode is open, so the ladder's
	// list rung never runs and unsupported deny rows are omitted with a widening
	// notice instead of refusing. It still deploys an engine, so it must refuse.
	denyOnly := axes
	denyOnly.Network = sandboxpolicy.NetworkRules{
		Mode:   sandboxpolicy.AccessModeOpen,
		Deny:   []sandboxpolicy.NetworkAllowEntry{{Domain: "blocked.example"}},
		Engine: sandboxpolicy.NetworkEngineProxy,
	}
	_, denyErr := PredictAccessEnforcement(
		Default(), sandboxpolicy.ImplementationTclaudeLayer, denyOnly, "", "linux",
	)
	require.Error(t, denyErr,
		"a deny-only proxy policy loses the same name authority and must refuse too")
	assert.Contains(t, denyErr.Error(), "proxy_engine_name_authority")

	// Parity: the identical filesystem grant under the packet gateway changes
	// nothing, because its DNS broker holds name authority with a resolver
	// socket present. Unset must likewise be untouched.
	for _, engine := range []sandboxpolicy.NetworkEngine{
		sandboxpolicy.NetworkEnginePacket,
		sandboxpolicy.NetworkEngineUnset,
	} {
		other := axes
		other.Network = proxyEngineRules(
			sandboxpolicy.NetworkAllowEntry{Domain: "example.com", Ports: []int{443}},
		)
		other.Network.Engine = engine
		predicted, err := PredictAccessEnforcement(
			Default(), sandboxpolicy.ImplementationTclaudeLayer, other, "", "linux",
		)
		require.NoErrorf(t, err, "engine %q must be unaffected", engine)
		assert.Equal(t, EnforceFull, predicted.NetworkList)
	}

	// And a proxy policy whose grants reach no resolver keeps working, so the
	// refusal is a signal rather than a blanket ban on the engine.
	innocent := axes
	innocent.Filesystem = []sandboxpolicy.FilesystemGrant{
		{Path: "/srv/toolchain", Access: sandboxpolicy.AccessRead},
	}
	predicted, err := PredictAccessEnforcement(
		Default(), sandboxpolicy.ImplementationTclaudeLayer, innocent, "", "linux",
	)
	require.NoError(t, err)
	assert.Equal(t, sandboxpolicy.NetworkEngineProxy, predicted.NetworkEngine)
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

// TestProxyEngineActivationIsScopedToItsEvidence is §8.3's rule expressed as a
// test: a cell is Full exactly where a named green smoke backs it, and nowhere
// else. The three boundaries below are the ways an activation silently spreads
// past its evidence — to an unmeasured harness, to an unmeasured platform, or
// to the engine that was never the subject of the smoke.
func TestProxyEngineActivationIsScopedToItsEvidence(t *testing.T) {
	rules := proxyEngineRules(
		sandboxpolicy.NetworkAllowEntry{Domain: "example.com", Ports: []int{443}},
	)
	axes := sandboxpolicy.ResolvedAxes{Network: rules}

	// Activated: every harness in the record. Codex joins Claude Code here in
	// TCL-888 on the evidence TCL-884 already named, and it is asserted through
	// the same predicted row rather than through a Claude-shaped assumption —
	// the branch that sets these cells is harness-agnostic apart from the
	// record lookup, and this loop is what holds it that way.
	for _, activated := range activatedProxyEngineHarnesses(t) {
		predicted, err := PredictAccessEnforcement(
			activated, sandboxpolicy.ImplementationTclaudeLayer, axes, "", "linux",
		)
		require.NoErrorf(t, err, "harness %s", activated.Name)
		assert.Equalf(t, EnforceFull, predicted.NetworkList,
			"%s has green named smokes and must be enforced", activated.Name)
		assert.NotEmptyf(t, ProxyEngineActivationSmokes(activated.Name),
			"%s must record the smokes its cells rest on", activated.Name)
		// §5.3 on the activated side, and the mirror of boundary 1 below: the
		// not-activated sentence has retired for this harness while the carriage
		// notice stays, because activation changes what is enforced and not what
		// the engine carries.
		assert.NotContainsf(t, predicted.NetworkEngineDetail,
			ProxyEngineNotActivatedNotice,
			"%s is activated and must not still say it is not", activated.Name)
		assert.Containsf(t, predicted.NetworkEngineDetail,
			ProxyEngineCarriageNotice,
			"%s still discloses what the engine carries", activated.Name)
		// §5.1's mirror image, both halves. Rating CIDR Full here would be the
		// flattering-but-wrong direction, and it is the one worth pinning.
		host, ok := networkSelectorCapability(predicted.NetworkSelectors,
			string(sandboxpolicy.NetworkSelectorHost))
		require.True(t, ok)
		assert.Equal(t, EnforceFull, host.Level)
		cidr, ok := networkSelectorCapability(predicted.NetworkSelectors,
			string(sandboxpolicy.NetworkSelectorCIDR))
		require.True(t, ok)
		assert.Equal(t, EnforcePartial, cidr.Level,
			"CIDR drops to Partial under an L7 view and must not be over-claimed")
		// Deny names are Partial, not Full. The engine has no DNS broker to
		// bypass, but it decides on the identity the client states, and a name
		// deny is never matched against an IP literal.
		denyHost, ok := networkSelectorCapability(predicted.NetworkDenySelectors,
			string(sandboxpolicy.NetworkSelectorHost))
		require.True(t, ok)
		assert.Equal(t, EnforcePartial, denyHost.Level,
			"a name deny does not cover a client that asks by address")
		assert.Contains(t, denyHost.Detail, "asks for the address literally")
	}

	// Boundary 1: THE CELLS FOLLOW THE RECORD, over every registered harness.
	//
	// This used to be a loop over the harnesses that had NO record — first
	// Codex and OpenCode, then OpenCode alone. TCL-891 activates the last of
	// them, and that is exactly when the old shape became dangerous: a loop
	// whose subject set empties on success asserts nothing and passes silently.
	// Nobody would have seen the rule stop being tested, because the way it
	// stops being tested is that it goes green.
	//
	// So the coupling is asserted instead, over a subject set that grows with
	// the registry: for every registered harness on Linux, the cells are
	// enforced exactly when the record backs them. The expectation is restated
	// from the exported accessor rather than read from proxyEngineActivated,
	// which would make it tautological — the point is that the RECORD and the
	// CELLS agree, so the record must be consulted independently of the
	// predicate that reads it.
	activatedRows := 0
	for _, name := range Names() {
		predicted, err := PredictAccessEnforcement(
			MustGet(name), sandboxpolicy.ImplementationTclaudeLayer,
			axes, "", "linux",
		)
		require.NoErrorf(t, err, "harness %s", name)
		if len(ProxyEngineActivationSmokes(name)) == 0 {
			assert.Equalf(t, EnforceNone, predicted.NetworkList,
				"%s has no record and must stay unenforced", name)
			assert.Containsf(t, predicted.NetworkEngineDetail,
				ProxyEngineNotActivatedNotice,
				"%s must still disclose that it is not activated", name)
			continue
		}
		activatedRows++
		assert.Equalf(t, EnforceFull, predicted.NetworkList,
			"%s has a record and must be enforced", name)
		assert.NotContainsf(t, predicted.NetworkEngineDetail,
			ProxyEngineNotActivatedNotice,
			"%s has a record and must not say it is unactivated", name)
	}
	require.Positive(t, activatedRows,
		"the activation record is empty, so this coupling has no subject at all")

	// AND THE OTHER DIRECTION, WHICH NO REGISTERED HARNESS CAN DEMONSTRATE ON
	// LINUX ANY MORE. With TCL-891 every registered harness has a Linux row, so
	// the loop above can no longer exercise "a harness the record does not
	// mention is not activated".
	//
	// It is asserted on the predicate, where it is real and permanent: a name
	// the record does not mention is not activated. That is what a future
	// harness registered before its evidence exists will rely on.
	assert.False(t, proxyEngineActivated("harness-with-no-activation-record", "linux"),
		"the record lookup must be fail-closed for a name it does not mention")

	// A ROW WITH NO SMOKES IS THE SAME BUG WEARING A ROW. proxyEngineActivated
	// keys on presence, so an entry added with an empty slice would activate
	// cells while naming no evidence at all — the exact thing the record exists
	// to prevent, and the one authoring mistake the coupling above cannot see.
	for goos, rows := range proxyEngineActivatedSmokes {
		for name := range rows {
			assert.NotEmptyf(t, ProxyEngineActivationSmokesForPlatform(name, goos),
				"%s/%s has a row but names no smoke, so its cells rest on nothing", goos, name)
		}
	}

	// Boundary 2: Darwin has its own evidence rows. Every harness activates at
	// Partial because TCL-917 caps the Seatbelt floor, while OpenCode cites its
	// distinct agentd-owned server smoke rather than the plain-CLI smoke.
	for _, name := range []string{DefaultName, CodexName} {
		activated := MustGet(name)
		darwin, err := PredictAccessEnforcement(
			activated, sandboxpolicy.ImplementationTclaudeLayer, axes, "", "darwin",
		)
		require.NoErrorf(t, err, "harness %s", name)
		assert.Equalf(t, EnforcePartial, darwin.NetworkList,
			"Darwin proxy cells for %s must retain the TCL-917 Partial cap", name)
		assert.Contains(t, darwin.NetworkListCondition, SeatbeltProxyFloorCondition)
		assert.Truef(t, proxyEngineActivated(name, "darwin"),
			"%s must be backed by the Darwin activation record", name)
		assert.Equal(t, []string{"TestPinnedProxyHarnessCooperationDarwin"},
			ProxyEngineActivationSmokesForPlatform(name, "darwin"))
	}
	assert.True(t, proxyEngineActivated(OpenCodeName, "darwin"))
	openCodeDarwin, err := PredictAccessEnforcement(
		MustGet(OpenCodeName), sandboxpolicy.ImplementationTclaudeLayer,
		axes, "", "darwin",
	)
	require.NoError(t, err)
	assert.Equal(t, EnforcePartial, openCodeDarwin.NetworkList)
	assert.Contains(t, openCodeDarwin.NetworkListCondition,
		SeatbeltProxyFloorCondition)
	assert.Contains(t, openCodeDarwin.NetworkListCondition,
		OpenCodeFilteredExplicitProviderCaveat)
	assert.Equal(t, []string{"TestOpenCodeProxyCooperationDarwin"},
		ProxyEngineActivationSmokesForPlatform(OpenCodeName, "darwin"))

	// Boundary 3: the packet gateway is untouched. Its DNS caveat is the marker
	// — if the proxy branch had leaked into it, that caveat would be gone.
	packetRules := rules
	packetRules.Engine = sandboxpolicy.NetworkEnginePacket
	packet, err := PredictAccessEnforcement(
		Default(), sandboxpolicy.ImplementationTclaudeLayer,
		sandboxpolicy.ResolvedAxes{Network: packetRules}, "", "linux",
	)
	require.NoError(t, err)
	assert.Equal(t, EnforceFull, packet.NetworkList)
	packetCIDR, ok := networkSelectorCapability(packet.NetworkSelectors,
		string(sandboxpolicy.NetworkSelectorCIDR))
	require.True(t, ok)
	assert.Equal(t, EnforceFull, packetCIDR.Level,
		"the packet gateway rates CIDR Full and this change does not touch it")
	assert.Contains(t, packet.NetworkListCondition, "pasta")
}

// TestActivatedProxyEngineStopsWideningToOpen is the operator-visible half of
// the flip. While the cells were unenforced, an authored proxy policy was
// widened to open with a persisted warning; that must stop for an activated
// configuration, and the launch must render the authored list instead.
func TestActivatedProxyEngineStopsWideningToOpen(t *testing.T) {
	axes := sandboxpolicy.ResolvedAxes{
		Network: proxyEngineRules(
			sandboxpolicy.NetworkAllowEntry{Domain: "example.com", Ports: []int{443}},
		),
	}
	rendered, notices, err := PlanAccessEnforcement(
		axes, accessEnforcementFromTable(mustEngineTableRow(t, axes)),
	)
	require.NoError(t, err)
	assert.Equal(t, sandboxpolicy.AccessModeList, rendered.Network.Mode,
		"an activated proxy policy must reach the launch as a list, not widened open")
	assert.Equal(t, sandboxpolicy.NetworkEngineProxy, rendered.Network.Engine)
	for _, notice := range notices {
		assert.NotEqualf(t, "no_mechanism", notice.Reason,
			"the widen-to-open warning must stop firing once the cells are activated: %s",
			notice.Detail)
	}
}

// TestProxyEngineDenyNameRatingMatchesTheEvaluator is the test whose absence let
// a Full rating stand: every other proxy assertion runs against a list policy,
// where the escape needs a cidr row to bite, so the rating was only ever pinned
// in the shape where it was least wrong.
//
// It compares the RATING against the real evaluator rather than against a
// second opinion about the rating. A name deny that the evaluator does not
// apply to a literal cannot be rendered as fully enforced, whichever baseline
// the policy uses.
//
// TCL-888 runs it for every activated harness rather than only the first one.
// The evaluator is harness-blind, so a harness whose cells were flipped without
// its ratings being checked against it is exactly how the over-claim above got
// in: the rating has to be re-derived from the evaluator per row that renders
// it, not inherited from the row that was checked.
func TestProxyEngineDenyNameRatingMatchesTheEvaluator(t *testing.T) {
	const denied = "evil.example.com"
	const deniedAddr = "93.184.216.34"

	for _, testCase := range []struct {
		name  string
		rules sandboxpolicy.NetworkRules
	}{
		{
			// Default-allow: the literal is simply allowed.
			name: "open baseline",
			rules: sandboxpolicy.NetworkRules{
				Mode:   sandboxpolicy.AccessModeOpen,
				Deny:   []sandboxpolicy.NetworkAllowEntry{{Host: denied}},
				Engine: sandboxpolicy.NetworkEngineProxy,
			},
		},
		{
			// Allowlist: the literal is allowed because a cidr row covers it,
			// so the deny is escaped without any DNS trickery at all.
			name: "list baseline with a covering cidr rule",
			rules: sandboxpolicy.NetworkRules{
				Mode: sandboxpolicy.AccessModeList,
				Allow: []sandboxpolicy.NetworkAllowEntry{
					{CIDR: "93.184.216.0/24", Ports: []int{443}},
				},
				Deny:   []sandboxpolicy.NetworkAllowEntry{{Host: denied}},
				Engine: sandboxpolicy.NetworkEngineProxy,
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			evaluator, err := sandboxproxy.NewEvaluator(testCase.rules)
			require.NoError(t, err)

			byName, err := sandboxproxy.ParseTarget(denied, 443)
			require.NoError(t, err)
			require.False(t, evaluator.Evaluate(byName).Allowed(),
				"the deny must hold for the name it was authored against")

			byLiteral, err := sandboxproxy.ParseTarget(deniedAddr, 443)
			require.NoError(t, err)
			escaped := evaluator.Evaluate(byLiteral).Allowed()

			for _, activated := range activatedProxyEngineHarnesses(t) {
				predicted, err := PredictAccessEnforcement(
					activated, sandboxpolicy.ImplementationTclaudeLayer,
					sandboxpolicy.ResolvedAxes{Network: testCase.rules}, "", "linux",
				)
				require.NoErrorf(t, err, "harness %s", activated.Name)
				capability, ok := networkSelectorCapability(
					predicted.NetworkDenySelectors,
					string(sandboxpolicy.NetworkSelectorHost))
				require.Truef(t, ok, "harness %s", activated.Name)

				if escaped {
					assert.NotEqualf(t, EnforceFull, capability.Level,
						"the evaluator carried %s:443 past a deny on %s, so %s's cell must not claim full enforcement",
						deniedAddr, denied, activated.Name)
					assert.NotEmptyf(t, capability.Detail,
						"a partial rating must say what it does not cover (%s)",
						activated.Name)
				}
			}
		})
	}
}

// TestProxyEngineDenyLoopbackRatingMatchesTheEvaluator is the other half of the
// deny row, and it exists because the shape LOOKS like the over-claim its
// neighbour above caught: loopback is rated Full while the two name selectors
// beside it are Partial, so a reader scanning the cells sees the flattering
// rating in the position where the flattering rating was wrong last time.
//
// It is right here, and the reason is a property of the evaluator rather than
// an argument about the engine: host loopback has several spellings and the
// evaluator folds every one of them into a single identity before matching, so
// there is no by-address restatement to walk past a deny the way there is for a
// name. Asserted against that evaluator, in both baselines, for every activated
// harness — the same shape that would have caught the name over-claim.
func TestProxyEngineDenyLoopbackRatingMatchesTheEvaluator(t *testing.T) {
	denyLoopback := []sandboxpolicy.NetworkAllowEntry{{Loopback: true}}

	for _, testCase := range []struct {
		name  string
		rules sandboxpolicy.NetworkRules
	}{
		{
			name: "open baseline",
			rules: sandboxpolicy.NetworkRules{
				Mode:   sandboxpolicy.AccessModeOpen,
				Deny:   denyLoopback,
				Engine: sandboxpolicy.NetworkEngineProxy,
			},
		},
		{
			name: "list baseline",
			rules: sandboxpolicy.NetworkRules{
				Mode: sandboxpolicy.AccessModeList,
				Allow: []sandboxpolicy.NetworkAllowEntry{
					{Domain: "example.com", Ports: []int{443}},
				},
				Deny:   denyLoopback,
				Engine: sandboxpolicy.NetworkEngineProxy,
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			evaluator, err := sandboxproxy.NewEvaluator(testCase.rules)
			require.NoError(t, err)

			// Every spelling a client could state, including the one an escape
			// would use: the literal rather than the name.
			//
			// The verdict is compared, not just Allowed(). Host loopback is the
			// one destination no baseline ever reaches on its own — an open
			// baseline explicitly declines to default-accept it — so "not
			// allowed" is the answer here whether the deny row matched or
			// matched nothing at all. Only VerdictDeniedByRule distinguishes
			// the rating's actual premise from that ambient refusal.
			for _, spelling := range []string{"localhost", "127.0.0.1", "::1"} {
				target, err := sandboxproxy.ParseTarget(spelling, 8080)
				require.NoErrorf(t, err, "spelling %s", spelling)
				assert.Equalf(t, sandboxproxy.VerdictDeniedByRule,
					evaluator.Evaluate(target).Verdict,
					"the loopback deny must MATCH the %s spelling, not merely leave it unauthorized",
					spelling)
			}

			for _, activated := range activatedProxyEngineHarnesses(t) {
				predicted, err := PredictAccessEnforcement(
					activated, sandboxpolicy.ImplementationTclaudeLayer,
					sandboxpolicy.ResolvedAxes{Network: testCase.rules}, "", "linux",
				)
				require.NoErrorf(t, err, "harness %s", activated.Name)
				capability, ok := networkSelectorCapability(
					predicted.NetworkDenySelectors,
					string(sandboxpolicy.NetworkSelectorLoopback))
				require.Truef(t, ok, "harness %s", activated.Name)
				assert.Equalf(t, EnforceFull, capability.Level,
					"the evaluator refused every loopback spelling, so %s's cell is Full and rating it down would understate the engine",
					activated.Name)
			}
		})
	}
}

// TestResolverConflictRefusalsAreAsymmetric pins the recorded asymmetry between
// the two resolver-reaching refusals: which Kind each carries, where each
// refuses, and which one an operator sees when a policy trips both.
//
// The asymmetry is deliberate and its rationale lives in one comment block at
// the socket conflict site in access_enforcement.go. This test is the half that
// makes changing it visible: a future edit that aligns the Kinds or reorders
// the two has to come here and say so.
func TestResolverConflictRefusalsAreAsymmetric(t *testing.T) {
	const resolverSocket = "/run/systemd/resolve/io.systemd.Resolve"
	network := proxyEngineRules(
		sandboxpolicy.NetworkAllowEntry{Domain: "example.com", Ports: []int{443}},
	)

	// The socket conflict is RECORDED on the row, not returned: resolving the
	// target succeeds, and the refusal lands at the unix_sockets list rung with
	// the socket axis named.
	socketOnly := sandboxpolicy.ResolvedAxes{
		Network: network,
		UnixSockets: sandboxpolicy.UnixSocketRules{
			Mode:  sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.SocketAllowEntry{{Path: resolverSocket}},
		},
	}
	caps, err := accessEnforcementForTargetForTest(
		Default(), sandboxpolicy.ImplementationTclaudeLayer,
		socketOnly, "", "linux",
	)
	require.NoError(t, err,
		"the socket conflict is recorded on the row so the rest of the target still resolves")
	require.Equal(t, EnforceNone, caps.socketList)
	require.NotEmpty(t, caps.socketListRefusal)

	_, _, planErr := PlanAccessEnforcement(socketOnly, caps)
	require.Error(t, planErr, "the ladder's socket rung is where it refuses")
	var socketCapabilityErr *SandboxCapabilityError
	require.ErrorAs(t, planErr, &socketCapabilityErr)
	assert.Equal(t, SandboxCapabilitySocketAllowlist, socketCapabilityErr.Kind,
		"the offending authority is the socket allow list, so the remedy is a socket-axis edit")
	assert.Contains(t, planErr.Error(), "proxy_engine_name_authority")

	// The filesystem conflict returns from the capability seam instead, with
	// the network axis named. Linux-only, as the refusal itself is.
	filesystemOnly := sandboxpolicy.ResolvedAxes{
		Network: network,
		Filesystem: []sandboxpolicy.FilesystemGrant{
			{Path: resolverSocket, Access: sandboxpolicy.AccessRead},
		},
	}
	_, filesystemErr := accessEnforcementForTargetForTest(
		Default(), sandboxpolicy.ImplementationTclaudeLayer,
		filesystemOnly, "", "linux",
	)
	require.Error(t, filesystemErr,
		"the filesystem conflict has no ladder rung to defer to and refuses at the seam")
	var filesystemCapabilityErr *SandboxCapabilityError
	require.ErrorAs(t, filesystemErr, &filesystemCapabilityErr)
	assert.Equal(t, SandboxCapabilityNetworkAllowlist,
		filesystemCapabilityErr.Kind)

	// Both at once: the filesystem refusal is the one that surfaces, because it
	// returns before the row reaches the ladder. It is the more general of the
	// two, so it is also the answer that stays correct if only one authored row
	// is fixed.
	both := sandboxpolicy.ResolvedAxes{
		Network:     network,
		UnixSockets: socketOnly.UnixSockets,
		Filesystem:  filesystemOnly.Filesystem,
	}
	_, bothErr := accessEnforcementForTargetForTest(
		Default(), sandboxpolicy.ImplementationTclaudeLayer, both, "", "linux",
	)
	require.Error(t, bothErr)
	var bothCapabilityErr *SandboxCapabilityError
	require.ErrorAs(t, bothErr, &bothCapabilityErr)
	assert.Equal(t, SandboxCapabilityNetworkAllowlist, bothCapabilityErr.Kind,
		"a policy tripping both refusals surfaces the filesystem one")
}

// activatedProxyEngineHarnesses derives the activated set FROM THE RECORD rather
// than listing it, so a harness added to the record joins every assertion that
// applies to activated harnesses without anyone remembering to update a slice.
//
// It refuses an empty set. A helper that quietly returned nothing would make
// every loop over it vacuous — the same silent-pass shape the boundary above
// was rewritten to avoid, one level down.
func activatedProxyEngineHarnesses(t *testing.T) []*Harness {
	t.Helper()
	var out []*Harness
	for _, name := range Names() {
		if len(ProxyEngineActivationSmokes(name)) > 0 {
			out = append(out, MustGet(name))
		}
	}
	require.NotEmpty(t, out,
		"the activation record is empty, so every assertion about activated harnesses would be vacuous")
	return out
}

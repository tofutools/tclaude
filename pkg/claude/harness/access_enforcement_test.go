package harness

import (
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestAccessEnforcementRungOneRequiresSandboxEvidence(t *testing.T) {
	h, err := Resolve(DefaultName)
	require.NoError(t, err)
	axes := sandboxpolicy.ResolvedAxes{
		Network: sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeList},
	}
	_, err = ResolveAccessEnforcement(
		h, sandboxpolicy.ImplementationHarnessBuiltin, axes,
		LaunchOSSandbox{}, ClaudeSandboxOff,
	)
	require.ErrorContains(t, err, `verdict is "off"`)

	_, err = ResolveAccessEnforcement(
		h, sandboxpolicy.ImplementationHarnessBuiltin, axes,
		LaunchOSSandbox{State: "on", Source: "lower settings tier", Unverified: true},
		ClaudeSandboxInherit,
	)
	require.ErrorContains(t, err, "higher-precedence Claude settings could not be verified")

	_, err = ResolveAccessEnforcement(
		h, sandboxpolicy.ImplementationTclaudeLayer, axes,
		LaunchOSSandbox{
			State: "on", Source: "partially enforced outer sandbox", Unverified: true,
		},
		ClaudeSandboxOff,
	)
	require.NoError(t, err,
		"outer implementations use Unverified for disclosed partial fidelity, not missing sandbox evidence")

	_, _, err = PlanAccessEnforcement(axes, AccessEnforcement{})
	require.ErrorContains(t, err, "not resolved through the sandbox implementation gate")
}

func TestBuiltinAccessEvidenceRejectsNonConfiningAndOpenCodeModes(t *testing.T) {
	codex, err := Resolve(CodexName)
	require.NoError(t, err)
	verdict, err := BuiltinLaunchOSSandboxForValidatedMode(codex, SandboxDangerFull)
	require.NoError(t, err)
	assert.Equal(t, "off", verdict.State)

	opencode, err := Resolve(OpenCodeName)
	require.NoError(t, err)
	_, err = BuiltinLaunchOSSandboxForValidatedMode(opencode, OpenCodeSandboxAccessControl)
	require.ErrorContains(t, err, "has no built-in OS sandbox")
}

func TestLinuxTclaudeLayerSocketCapabilitiesAreCombinationAware(t *testing.T) {
	h, err := Resolve(DefaultName)
	require.NoError(t, err)
	evidence := LaunchOSSandbox{State: "on", Source: "verified bwrap"}

	closed := sandboxpolicy.ResolvedAxes{
		Network:     sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeClosed},
		UnixSockets: sandboxpolicy.UnixSocketRules{Mode: sandboxpolicy.AccessModeClosed},
	}
	caps, err := accessEnforcementForTargetForTest(
		h, sandboxpolicy.ImplementationTclaudeLayer, closed, ClaudeSandboxOff, "linux",
	)
	require.NoError(t, err)
	assert.Equal(t, EnforcePartial, caps.socketClosed)
	_, notices, err := PlanAccessEnforcement(closed, caps)
	require.NoError(t, err)
	require.Len(t, notices, 1,
		"claiming Full or suppressing the partial disclosure must fail this guard")
	assert.Equal(t, sandboxpolicy.AccessNoticeEffectEnforcedWider, notices[0].Effect)
	assert.Contains(t, notices[0].Detail, "readable/writable directories")
	assert.Contains(t, notices[0].Detail, "remain reachable")
	assert.Contains(t, notices[0].Detail, "outside")

	explicitOpenSockets := sandboxpolicy.ResolvedAxes{
		Network:     sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeClosed},
		UnixSockets: sandboxpolicy.UnixSocketRules{Mode: sandboxpolicy.AccessModeOpen},
	}
	caps, err = accessEnforcementForTargetForTest(
		h, sandboxpolicy.ImplementationTclaudeLayer, explicitOpenSockets, ClaudeSandboxOff, "linux",
	)
	require.NoError(t, err)
	assert.Equal(t, EnforceNone, caps.socketOpen)
	_, _, err = PlanAccessEnforcement(explicitOpenSockets, caps)
	require.EqualError(t, err,
		`unix_sockets "open" cannot preserve ambient host socket visibility with closed network access on Linux tclaude-layer; `+
			`use a socket access list or leave unix_sockets unset`)

	unsetSockets := sandboxpolicy.ResolvedAxes{
		Network: sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeClosed},
	}
	caps, err = accessEnforcementForTargetForTest(
		h, sandboxpolicy.ImplementationTclaudeLayer, unsetSockets, ClaudeSandboxOff, "linux",
	)
	require.NoError(t, err)
	_, notices, err = PlanAccessEnforcement(unsetSockets, caps)
	require.NoError(t, err)
	assert.Empty(t, notices,
		"an unset socket axis must preserve the existing constructed-root behavior")

	closedNetworkSocketList := sandboxpolicy.ResolvedAxes{
		Network: sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeClosed},
		UnixSockets: sandboxpolicy.UnixSocketRules{
			Mode:  sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.SocketAllowEntry{{Path: "/tmp/service.sock"}},
		},
	}
	caps, err = accessEnforcementForTargetForTest(
		h, sandboxpolicy.ImplementationTclaudeLayer, closedNetworkSocketList, ClaudeSandboxOff, "linux",
	)
	require.NoError(t, err)
	assert.Equal(t, EnforcePartial, caps.socketList)
	rendered, notices, err := PlanAccessEnforcement(closedNetworkSocketList, caps)
	require.NoError(t, err)
	assert.Equal(t, closedNetworkSocketList.UnixSockets, rendered.UnixSockets)
	require.Len(t, notices, 1,
		"claiming Full or suppressing the partial disclosure must fail this guard")
	assert.Equal(t, "partial_mechanism", notices[0].Reason)
	assert.Equal(t, sandboxpolicy.AccessNoticeEffectEnforcedWider, notices[0].Effect)
	assert.Contains(t, notices[0].Detail, "listed Unix sockets are bound")
	assert.Contains(t, notices[0].Detail, "readable/writable directories")
	assert.Contains(t, notices[0].Detail, "remain reachable")
	assert.Contains(t, notices[0].Detail, "outside")

	// TCL-798: the constructed root no longer requires giving up host
	// networking, so both restricting tiers are enforceable here — Partial, and
	// permanently so, because abstract-namespace sockets are not filesystem
	// objects and the network namespace is shared.
	hostOpenClosedSockets := sandboxpolicy.ResolvedAxes{
		Network:     sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeOpen},
		UnixSockets: sandboxpolicy.UnixSocketRules{Mode: sandboxpolicy.AccessModeClosed},
	}
	caps, err = accessEnforcementForTargetForTest(
		h, sandboxpolicy.ImplementationTclaudeLayer, hostOpenClosedSockets, ClaudeSandboxOff, "linux",
	)
	require.NoError(t, err)
	assert.Equal(t, EnforcePartial, caps.socketClosed,
		"claiming Full here would deny the abstract-namespace remainder this posture cannot reach")
	rendered, notices, err = PlanAccessEnforcement(hostOpenClosedSockets, caps)
	require.NoError(t, err)
	assert.Equal(t, hostOpenClosedSockets.UnixSockets, rendered.UnixSockets,
		"an enforceable tier must survive the ladder rather than widening to open")
	require.Len(t, notices, 1)
	assert.Equal(t, "partial_mechanism", notices[0].Reason)
	assert.Equal(t, sandboxpolicy.AccessNoticeEffectEnforcedWider, notices[0].Effect)
	assert.Contains(t, notices[0].Detail, "abstract-namespace Unix sockets",
		"the day-one disclosure of the abstract-socket remainder is mandatory")
	assert.Contains(t, notices[0].Detail, "readable/writable directories")
	// The PID namespace is a REQUIREMENT of this posture's semantic, not a side
	// effect, so its consequence is disclosed in the same sentence family: an
	// operator authoring closed sockets under host networking also loses host
	// process visibility.
	assert.Contains(t, notices[0].Detail, "PID namespace")
	assert.Contains(t, notices[0].Detail, "cannot see or signal host processes")
	// Building the root also narrows what the agent can SEE. An operator who
	// authored only a SOCKET rule must not discover that by watching a launch
	// fail, so the filesystem consequence is disclosed here too.
	assert.Contains(t, notices[0].Detail, "no longer visible at all")
	// The posture is NAMED, not merely described, so an operator whose
	// pre-existing sockets+open profile changes behavior on upgrade can see
	// which boundary became active.
	assert.Contains(t, notices[0].Detail, "host-network constructed root")
	assert.Equal(t, "tclaude-layer bubblewrap (host-network constructed root)",
		caps.mechanism)
	assert.Contains(t, notices[0].Detail,
		"read-only OS surface it mounts (/usr, /bin, /sbin, /lib*, /etc, /opt)",
		"sockets on the static OS surface stay connectable; naming it is what makes the remainder honest")

	hostOpenSocketList := sandboxpolicy.ResolvedAxes{
		Network: sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeOpen},
		UnixSockets: sandboxpolicy.UnixSocketRules{
			Mode:  sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.SocketAllowEntry{{Path: "/tmp/service.sock"}},
		},
	}
	caps, err = accessEnforcementForTargetForTest(
		h, sandboxpolicy.ImplementationTclaudeLayer, hostOpenSocketList, ClaudeSandboxOff, "linux",
	)
	require.NoError(t, err)
	assert.Equal(t, EnforcePartial, caps.socketList)
	rendered, notices, err = PlanAccessEnforcement(hostOpenSocketList, caps)
	require.NoError(t, err)
	assert.Equal(t, hostOpenSocketList.UnixSockets, rendered.UnixSockets)
	require.Len(t, notices, 1)
	assert.Equal(t, "partial_mechanism", notices[0].Reason)
	assert.Contains(t, notices[0].Detail, "abstract-namespace Unix sockets")
	assert.Contains(t, notices[0].Detail, "cannot see or signal host processes")

	// No default-on: a profile that never authored the axis must launch in
	// exactly the posture it launched in before TCL-798, with no notice.
	hostOpenUnsetSockets := sandboxpolicy.ResolvedAxes{
		Network: sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeOpen},
	}
	caps, err = accessEnforcementForTargetForTest(
		h, sandboxpolicy.ImplementationTclaudeLayer, hostOpenUnsetSockets, ClaudeSandboxOff, "linux",
	)
	require.NoError(t, err)
	_, notices, err = PlanAccessEnforcement(hostOpenUnsetSockets, caps)
	require.NoError(t, err)
	assert.Empty(t, notices,
		"an unset socket axis must not acquire a constructed root under host-open network")

	// The stacked implementation composes this root with a harness-native inner
	// sandbox that has no smoke evidence for the combination, so it keeps the
	// pre-TCL-798 rating.
	caps, err = accessEnforcementForTargetForTest(
		h, sandboxpolicy.ImplementationStacked, hostOpenClosedSockets, ClaudeSandboxOff, "linux",
	)
	require.NoError(t, err)
	assert.Equal(t, EnforceNone, caps.socketClosed)

	// A deny entry renders the filtered posture instead of host-open; that
	// combination is rated by its own row, not by this one.
	hostOpenDeniedClosedSockets := hostOpenClosedSockets
	hostOpenDeniedClosedSockets.Network = sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeOpen,
		Deny: []sandboxpolicy.NetworkAllowEntry{{Host: "metadata.google.internal"}},
	}
	caps, err = accessEnforcementForTargetForTest(
		h, sandboxpolicy.ImplementationTclaudeLayer, hostOpenDeniedClosedSockets,
		ClaudeSandboxOff, "linux",
	)
	require.NoError(t, err)
	assert.Equal(t, EnforceNone, caps.socketClosed)

	// The public resolver consumes the same axes rather than returning a static
	// per-target capability descriptor. Unlike the table helper above it reads
	// the REAL platform, so its expectation is the platform's own: TCL-798's
	// constructed root is a Linux mount-namespace capability, and Darwin's
	// host-open renderer still wires no socket denies.
	wantResolved := EnforceNone
	if runtime.GOOS == "linux" {
		wantResolved = EnforcePartial
	}
	resolved, err := ResolveAccessEnforcement(
		h, sandboxpolicy.ImplementationTclaudeLayer, hostOpenSocketList,
		evidence, ClaudeSandboxOff,
	)
	require.NoError(t, err)
	assert.Equal(t, wantResolved, resolved.socketClosed)
}

func TestSocketListAdaptersPreserveRuledCombinationBoundaries(t *testing.T) {
	claude := Default()
	closedSocketList := sandboxpolicy.ResolvedAxes{
		Network: sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeClosed},
		UnixSockets: sandboxpolicy.UnixSocketRules{
			Mode:  sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.SocketAllowEntry{{Path: "/tmp/service.sock"}},
		},
	}
	darwinCaps, err := accessEnforcementForTargetForTest(
		claude, sandboxpolicy.ImplementationTclaudeLayer,
		closedSocketList, ClaudeSandboxOff, "darwin",
	)
	require.NoError(t, err)
	assert.Equal(t, EnforceFull, darwinCaps.socketList)
	rendered, notices, err := PlanAccessEnforcement(closedSocketList, darwinCaps)
	require.NoError(t, err)
	assert.Equal(t, closedSocketList.UnixSockets, rendered.UnixSockets)
	assert.Empty(t, notices)

	closedSocketOpen := sandboxpolicy.ResolvedAxes{
		Network:     sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeClosed},
		UnixSockets: sandboxpolicy.UnixSocketRules{Mode: sandboxpolicy.AccessModeOpen},
	}
	darwinCaps, err = accessEnforcementForTargetForTest(
		claude, sandboxpolicy.ImplementationTclaudeLayer,
		closedSocketOpen, ClaudeSandboxOff, "darwin",
	)
	require.NoError(t, err)
	_, notices, err = PlanAccessEnforcement(closedSocketOpen, darwinCaps)
	require.EqualError(t, err,
		"ambient unix-socket access is not yet enforceable under closed network access on macOS tclaude-layer; "+
			"leave unix_sockets unset (agentd only) or use open network access")
	assert.Empty(t, notices)

	hostOpenSocketList := closedSocketList
	hostOpenSocketList.Network.Mode = sandboxpolicy.AccessModeOpen
	darwinCaps, err = accessEnforcementForTargetForTest(
		claude, sandboxpolicy.ImplementationTclaudeLayer,
		hostOpenSocketList, ClaudeSandboxOff, "darwin",
	)
	require.NoError(t, err)
	rendered, notices, err = PlanAccessEnforcement(hostOpenSocketList, darwinCaps)
	require.NoError(t, err)
	assert.Equal(t, sandboxpolicy.AccessModeOpen, rendered.UnixSockets.Mode)
	require.Len(t, notices, 1)
	assert.Equal(t, sandboxpolicy.AccessNoticeEffectNotEnforced, notices[0].Effect)

	hostOpenSocketClosed := sandboxpolicy.ResolvedAxes{
		Network:     sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeOpen},
		UnixSockets: sandboxpolicy.UnixSocketRules{Mode: sandboxpolicy.AccessModeClosed},
	}
	darwinCaps, err = accessEnforcementForTargetForTest(
		claude, sandboxpolicy.ImplementationTclaudeLayer,
		hostOpenSocketClosed, ClaudeSandboxOff, "darwin",
	)
	require.NoError(t, err)
	assert.Equal(t, EnforceNone, darwinCaps.socketClosed,
		"removing combination awareness must not claim host-open Seatbelt closes Unix sockets")
	_, notices, err = PlanAccessEnforcement(hostOpenSocketClosed, darwinCaps)
	require.EqualError(t, err,
		`unix_sockets "closed" is not yet enforceable with open network access on macOS tclaude-layer; `+
			"close network access as well, use an access list (degrades, unenforced), or leave unix_sockets unset")
	assert.Empty(t, notices)

	codex, err := Resolve(CodexName)
	require.NoError(t, err)
	codexCaps, err := accessEnforcementForTargetForTest(
		codex, sandboxpolicy.ImplementationHarnessBuiltin,
		hostOpenSocketList, SandboxManagedProfile, "darwin",
	)
	require.NoError(t, err)
	assert.Equal(t, EnforceFull, codexCaps.socketList)
	rendered, notices, err = PlanAccessEnforcement(hostOpenSocketList, codexCaps)
	require.NoError(t, err)
	assert.Equal(t, hostOpenSocketList.UnixSockets, rendered.UnixSockets)
	require.Len(t, notices, 1)
	assert.Equal(t, "tools_only_scope", notices[0].Reason)

	unsetSockets := sandboxpolicy.ResolvedAxes{
		Network: sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeClosed},
	}
	for name, target := range map[string]struct {
		h              *Harness
		implementation sandboxpolicy.Implementation
		mode, goos     string
	}{
		"darwin tclaude-layer": {claude, sandboxpolicy.ImplementationTclaudeLayer, ClaudeSandboxOff, "darwin"},
		"codex managed":        {codex, sandboxpolicy.ImplementationHarnessBuiltin, SandboxManagedProfile, "darwin"},
	} {
		t.Run(name+" unset preserves baseline", func(t *testing.T) {
			caps, capsErr := accessEnforcementForTargetForTest(
				target.h, target.implementation, unsetSockets, target.mode, target.goos,
			)
			require.NoError(t, capsErr)
			rendered, unsetNotices, planErr := PlanAccessEnforcement(unsetSockets, caps)
			require.NoError(t, planErr)
			assert.Equal(t, unsetSockets.Network.Mode, rendered.Network.Mode)
			assert.Empty(t, rendered.Network.Allow)
			assert.Equal(t, sandboxpolicy.AccessModeUnset, rendered.UnixSockets.Mode)
			assert.Empty(t, rendered.UnixSockets.Allow)
			assert.Empty(t, unsetNotices)
		})
	}
}

func accessEnforcementForTargetForTest(
	h *Harness,
	implementation sandboxpolicy.Implementation,
	axes sandboxpolicy.ResolvedAxes,
	validatedBuiltinMode, goos string,
) (AccessEnforcement, error) {
	row, err := accessEnforcementTable(
		h, implementation, axes, validatedBuiltinMode, goos, false)
	if err != nil {
		return AccessEnforcement{}, err
	}
	return accessEnforcementFromTable(row), nil
}

func TestPlanAccessEnforcementOnlyWidensAndDisclosesScope(t *testing.T) {
	axes := sandboxpolicy.ResolvedAxes{
		Network: sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.NetworkAllowEntry{{
				Host: "api.example.com", Ports: []int{443},
			}},
		},
	}
	caps := AccessEnforcement{
		networkList: EnforcePartial,
		networkSelectors: []NetworkSelectorCapability{{
			Selector: "host", Level: EnforceFull,
		}},
		networkPorts: EnforceNone, scope: "tools-only",
		mechanism: "Claude Code sandbox", mcpBypass: true,
	}
	rendered, notices, err := PlanAccessEnforcement(axes, caps)
	require.NoError(t, err)
	assert.Equal(t, sandboxpolicy.AccessModeList, rendered.Network.Mode)
	assert.Empty(t, rendered.Network.Allow[0].Ports,
		"unsupported port constraints must widen to all ports, never drop the destination")
	require.Len(t, notices, 2)
	assert.Equal(t, "ports_unsupported", notices[0].Reason)
	assert.Contains(t, notices[0].Detail, "tools-only scope")
	assert.Contains(t, notices[0].Detail, "MCP servers bypass")
	assert.Equal(t, "tools_only_scope", notices[1].Reason)
}

func TestPlanAccessEnforcementOmitsUnsupportedDeniesIndividually(t *testing.T) {
	axes := sandboxpolicy.ResolvedAxes{Network: sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeOpen,
		Deny: []sandboxpolicy.NetworkAllowEntry{
			{CIDR: "192.0.2.0/24"},
			{Domain: "blocked.example", Ports: []int{443}},
			{Host: "unsupported.example"},
		},
	}}
	caps := AccessEnforcement{
		networkDenySelectors: []NetworkSelectorCapability{
			{Selector: string(sandboxpolicy.NetworkSelectorCIDR), Level: EnforceFull},
			{Selector: string(sandboxpolicy.NetworkSelectorDomain), Level: EnforceFull},
		},
		networkDenyPorts: EnforceNone,
		mechanism:        "test gateway",
		scope:            "process",
	}
	rendered, notices, err := PlanAccessEnforcement(axes, caps)
	require.NoError(t, err)
	assert.Equal(t, []sandboxpolicy.NetworkAllowEntry{{
		CIDR: "192.0.2.0/24",
	}}, rendered.Network.Deny)
	require.Len(t, notices, 2)
	assert.Equal(t, "deny_selector_unsupported", notices[0].Reason)
	assert.Equal(t, []int{2}, notices[0].Entries)
	assert.Equal(t, "deny_ports_unsupported", notices[1].Reason)
	assert.Equal(t, []int{1}, notices[1].Entries)
	assert.NotContains(t, rendered.Network.Deny,
		sandboxpolicy.NetworkAllowEntry{Domain: "blocked.example"},
		"an unsupported port-scoped deny must never widen to all ports")
}

func TestFilteredNetworkCapabilityMatrixFlipsOnlySmokeBackedCells(t *testing.T) {
	axes := sandboxpolicy.ResolvedAxes{
		Network: sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeList},
	}
	targets := []struct {
		name           string
		harness        *Harness
		implementation sandboxpolicy.Implementation
		mode           string
	}{
		{"Claude builtin", Default(), sandboxpolicy.ImplementationHarnessBuiltin, ClaudeSandboxOn},
		{"Codex builtin", MustGet(CodexName), sandboxpolicy.ImplementationHarnessBuiltin, SandboxManagedProfile},
		{"Claude tclaude", Default(), sandboxpolicy.ImplementationTclaudeLayer, ClaudeSandboxOff},
		{"Codex tclaude", MustGet(CodexName), sandboxpolicy.ImplementationTclaudeLayer, SandboxDangerFull},
		{"OpenCode tclaude", MustGet(OpenCodeName), sandboxpolicy.ImplementationTclaudeLayer, OpenCodeSandboxTclaudeLayer},
		{"Claude stacked", Default(), sandboxpolicy.ImplementationStacked, ClaudeSandboxOn},
		{"Codex stacked", MustGet(CodexName), sandboxpolicy.ImplementationStacked, SandboxManagedProfile},
	}
	for _, target := range targets {
		for _, platform := range []string{"linux", "darwin"} {
			t.Run(target.name+"/"+platform, func(t *testing.T) {
				row, err := accessEnforcementTable(
					target.harness, target.implementation, axes,
					target.mode, platform,
					true,
				)
				require.NoError(t, err)
				want := EnforceNone
				if platform == "linux" &&
					target.implementation == sandboxpolicy.ImplementationTclaudeLayer &&
					(target.harness.Name == DefaultName ||
						target.harness.Name == CodexName ||
						target.harness.Name == OpenCodeName) {
					want = EnforceFull
				}
				assert.Equal(t, want, row.NetworkList,
					"only the Linux tclaude-layer harness cells have executing CI smokes")
				denyWant := EnforceNone
				if platform == "linux" &&
					target.implementation == sandboxpolicy.ImplementationTclaudeLayer &&
					(target.harness.Name == DefaultName ||
						target.harness.Name == CodexName ||
						target.harness.Name == OpenCodeName) {
					denyWant = EnforceFull
				}
				assert.Equal(t, denyWant, row.NetworkDenyPorts,
					"deny activates only in the Linux tclaude-layer cells whose own executing CI smoke covers deny")
				if denyWant == EnforceNone {
					assert.Empty(t, row.NetworkDenySelectors,
						"an inactive cell must not advertise deny selectors at all")
				}
				for _, capability := range row.NetworkDenySelectors {
					assert.Equal(t, denyWant, capability.Level)
				}
			})
		}
	}

	row, err := accessEnforcementTable(
		Default(), sandboxpolicy.ImplementationTclaudeLayer, axes,
		ClaudeSandboxOff, "linux", false,
	)
	require.NoError(t, err)
	assert.Equal(t, EnforceNone, row.NetworkList,
		"a host-open verdict must not mint filtered enforcement")
	assert.Empty(t, row.NetworkDenySelectors)
	assert.Equal(t, EnforceNone, row.NetworkDenyPorts)
}

func TestLinuxTclaudeLayerDefaultAllowRatesOnlyDNSDeniesPartial(t *testing.T) {
	axes := sandboxpolicy.ResolvedAxes{
		Network: sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeOpen,
			Deny: []sandboxpolicy.NetworkAllowEntry{
				{Domain: "blocked.example"},
				{CIDR: "192.0.2.0/24"},
			},
		},
	}
	for _, harnessName := range []string{DefaultName, CodexName} {
		t.Run(harnessName, func(t *testing.T) {
			row, err := accessEnforcementTable(
				MustGet(harnessName),
				sandboxpolicy.ImplementationTclaudeLayer,
				axes, ClaudeSandboxOff, "linux", true,
			)
			require.NoError(t, err)
			domain, ok := networkSelectorCapability(
				row.NetworkDenySelectors,
				string(sandboxpolicy.NetworkSelectorDomain),
			)
			require.True(t, ok)
			assert.Equal(t, EnforcePartial, domain.Level)
			assert.Equal(t,
				FilteredNetworkDNSDenyDefaultAllowCaveat,
				domain.Detail)
			cidr, ok := networkSelectorCapability(
				row.NetworkDenySelectors,
				string(sandboxpolicy.NetworkSelectorCIDR),
			)
			require.True(t, ok)
			assert.Equal(t, EnforceFull, cidr.Level)
			assert.Equal(t, EnforceFull, row.NetworkDenyPorts)
		})
	}
}

func TestLinuxTclaudeLayerDenyCapabilityDrivesPredictionAndLaunchPlan(t *testing.T) {
	axes := sandboxpolicy.ResolvedAxes{
		Network: sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeOpen,
			Deny: []sandboxpolicy.NetworkAllowEntry{
				{Domain: "blocked.example", Ports: []int{443}},
				{CIDR: "192.0.2.0/24"},
			},
		},
	}
	modes := map[string]string{
		DefaultName:  ClaudeSandboxOff,
		CodexName:    SandboxDangerFull,
		OpenCodeName: OpenCodeSandboxTclaudeLayer,
	}
	for _, harnessName := range []string{DefaultName, CodexName, OpenCodeName} {
		t.Run(harnessName, func(t *testing.T) {
			row, err := accessEnforcementTable(
				MustGet(harnessName),
				sandboxpolicy.ImplementationTclaudeLayer,
				axes, modes[harnessName], "linux", true,
			)
			require.NoError(t, err)

			predicted := predictedAccessEnforcementFromTable(row)
			predictedRows := DescribePredictedNetworkDenyEntries(
				axes.Network.Deny, predicted)
			require.Len(t, predictedRows, 2)
			assert.Equal(t, AccessPredictionEnforcedPartial,
				predictedRows[0].Outcome)
			assert.Contains(t, predictedRows[0].Detail,
				FilteredNetworkDNSDenyDefaultAllowCaveat)
			assert.Equal(t, AccessPredictionEnforced,
				predictedRows[1].Outcome)

			rendered, notices, err := PlanAccessEnforcement(
				axes, accessEnforcementFromTable(row))
			require.NoError(t, err)
			assert.Equal(t, axes, rendered,
				"partial deny support must retain the exact deny row")
			require.Len(t, notices, 1)
			assert.Equal(t, "deny_selector_partial", notices[0].Reason)
			assert.Equal(t, []int{0}, notices[0].Entries)
		})
	}

	// The OpenCode local presets stay outside the activation: their filtered
	// list is refused at launch for want of an explicit provider, so their deny
	// rows must keep
	// disclosing omission rather than inheriting the flipped cell.
	localPresetAxes := sandboxpolicy.ResolvedAxes{
		Network: sandboxpolicy.NetworkRules{
			Mode:  sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.NetworkAllowEntry{{Loopback: true}},
			Deny:  axes.Network.Deny,
		},
	}
	presetRow, err := accessEnforcementTable(
		MustGet(OpenCodeName),
		sandboxpolicy.ImplementationTclaudeLayer,
		localPresetAxes, OpenCodeSandboxTclaudeLayer, "linux", true,
	)
	require.NoError(t, err)
	presetRows := DescribePredictedNetworkDenyEntries(
		localPresetAxes.Network.Deny,
		predictedAccessEnforcementFromTable(presetRow))
	require.Len(t, presetRows, 2)
	for _, predictedRow := range presetRows {
		assert.Equal(t, AccessPredictionNotEnforced, predictedRow.Outcome)
		assert.Equal(t, PredictedNetworkDenyNotEnforcedDetail,
			predictedRow.Detail)
	}
	// That preset never reaches a plan at all: its filtered list is refused.
	_, _, err = PlanAccessEnforcement(
		localPresetAxes, accessEnforcementFromTable(presetRow))
	require.ErrorContains(t, err, "unsupported_filtered_model_transport")

	// A cell without any executing deny smoke still omits each deny row.
	row, err := accessEnforcementTable(
		Default(), sandboxpolicy.ImplementationTclaudeLayer,
		axes, ClaudeSandboxOff, "darwin", true,
	)
	require.NoError(t, err)
	rendered, notices, err := PlanAccessEnforcement(
		axes, accessEnforcementFromTable(row))
	require.NoError(t, err)
	assert.Empty(t, rendered.Network.Deny,
		"a cell without deny capability must omit deny rows")
	require.Len(t, notices, 1)
	assert.Equal(t, "deny_selector_unsupported", notices[0].Reason)
	assert.Equal(t, []int{0, 1}, notices[0].Entries)
}

func TestCodexBuiltinFilteredNetworkPredictionDisclosesUnavailableCapability(t *testing.T) {
	axes := sandboxpolicy.ResolvedAxes{
		Network: sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.NetworkAllowEntry{{
				Domain: "api.openai.com", Ports: []int{443},
			}},
		},
	}
	codex := MustGet(CodexName)
	for _, platform := range []string{"linux", "darwin"} {
		t.Run(platform, func(t *testing.T) {
			prediction, err := PredictAccessEnforcement(
				codex, sandboxpolicy.ImplementationHarnessBuiltin,
				axes, SandboxManagedProfile, platform,
			)
			require.NoError(t, err)
			assert.Equal(t, EnforceNone, prediction.NetworkList)
			assert.Equal(t, CodexBuiltinFilteredNetworkDisclosure,
				prediction.NetworkListUnavailableDetail)

			axis := DescribePredictedAccess(axes, prediction).Network
			assert.Equal(t, AccessPredictionNotEnforced, axis.Outcome)
			assert.Equal(t, CodexBuiltinFilteredNetworkDisclosure, axis.Detail)

			rows := DescribePredictedNetworkEntries(axes.Network, prediction)
			require.Len(t, rows, 1)
			assert.Equal(t, AccessPredictionNotEnforced, rows[0].Outcome)
			assert.Equal(t, CodexBuiltinFilteredNetworkDisclosure, rows[0].Detail)

			launchRow, err := accessEnforcementTable(
				codex, sandboxpolicy.ImplementationHarnessBuiltin,
				axes, SandboxManagedProfile, platform, true,
			)
			require.NoError(t, err)
			assert.Equal(t, EnforceNone, launchRow.NetworkList)
			rendered, notices, err := PlanAccessEnforcement(
				axes, accessEnforcementFromTable(launchRow),
			)
			require.NoError(t, err)
			assert.Equal(t, sandboxpolicy.AccessModeOpen, rendered.Network.Mode,
				"the disclosure must not activate or narrow launch enforcement")
			require.Len(t, notices, 1)
			assert.Equal(t, "no_mechanism", notices[0].Reason)
		})
	}

	claudePrediction, err := PredictAccessEnforcement(
		Default(), sandboxpolicy.ImplementationHarnessBuiltin,
		axes, ClaudeSandboxOn, "linux",
	)
	require.NoError(t, err)
	assert.Empty(t, claudePrediction.NetworkListUnavailableDetail,
		"the Codex-specific disclosure must not attach to other None rows")
	assert.Contains(t, DescribePredictedAccess(axes, claudePrediction).Network.Detail,
		"no filtered-egress applier exists")
}

func TestSandboxOffPredictionNeverCreditsBuiltinEnforcement(t *testing.T) {
	axes := sandboxpolicy.ResolvedAxes{
		Network: sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeClosed},
		UnixSockets: sandboxpolicy.UnixSocketRules{
			Mode: sandboxpolicy.AccessModeClosed,
		},
	}
	for _, harnessName := range []string{DefaultName, CodexName, OpenCodeName} {
		t.Run(harnessName, func(t *testing.T) {
			prediction, err := PredictAccessEnforcement(
				MustGet(harnessName), sandboxpolicy.ImplementationOff,
				axes, "", "linux",
			)
			require.NoError(t, err)
			assert.Equal(t, "sandbox off", prediction.Mechanism)
			assert.Equal(t, EnforceNone, prediction.NetworkClosed)
			assert.Equal(t, EnforceNone, prediction.SocketClosed)
		})
	}
}

func TestM2bFilteredPredictionDisclosesLivePrerequisiteCondition(t *testing.T) {
	axes := sandboxpolicy.ResolvedAxes{
		Network: sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.NetworkAllowEntry{{
				CIDR: "192.0.2.0/24", Ports: []int{443},
			}},
		},
	}
	caps, err := PredictAccessEnforcement(
		Default(), sandboxpolicy.ImplementationTclaudeLayer,
		axes, ClaudeSandboxOff, "linux",
	)
	require.NoError(t, err)
	predicted := DescribePredictedAccess(axes, caps).Network
	assert.Equal(t, AccessPredictionEnforced, predicted.Outcome)
	assert.Contains(t, predicted.Detail, "At launch, bubblewrap, pasta, and nft must pass live checks")
	assert.Contains(t, predicted.Detail, "pasta")
	assert.Contains(t, predicted.Detail, "nft")
	assert.Contains(t, predicted.Detail, "outbound traffic is open")
}

func TestM3OpenCodeFilteredPredictionAndReadyPlanActivate(t *testing.T) {
	axes := sandboxpolicy.ResolvedAxes{
		Network: sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.NetworkAllowEntry{{
				CIDR: "192.0.2.0/24", Ports: []int{443},
			}},
		},
	}
	openCode := MustGet(OpenCodeName)
	prediction, err := PredictAccessEnforcement(
		openCode, sandboxpolicy.ImplementationTclaudeLayer,
		axes, OpenCodeSandboxTclaudeLayer, "linux",
	)
	require.NoError(t, err)
	preview := DescribePredictedAccess(axes, prediction).Network
	assert.Equal(t, AccessPredictionEnforced, preview.Outcome)
	assert.Contains(t, preview.Detail, "At launch, bubblewrap, pasta, and nft must pass live checks")

	row, err := accessEnforcementTable(
		openCode, sandboxpolicy.ImplementationTclaudeLayer, axes,
		OpenCodeSandboxTclaudeLayer, "linux", true,
	)
	require.NoError(t, err)
	rendered, notices, err := PlanAccessEnforcement(
		axes, accessEnforcementFromTable(row))
	require.NoError(t, err)
	assert.Equal(t, sandboxpolicy.AccessModeList, rendered.Network.Mode)
	assert.Empty(t, notices)

	row, err = accessEnforcementTable(
		openCode, sandboxpolicy.ImplementationTclaudeLayer, axes,
		OpenCodeSandboxTclaudeLayer, "linux", false,
	)
	require.NoError(t, err)
	rendered, notices, err = PlanAccessEnforcement(
		axes, accessEnforcementFromTable(row))
	require.NoError(t, err)
	assert.Equal(t, sandboxpolicy.AccessModeOpen, rendered.Network.Mode)
	require.Len(t, notices, 1)
	assert.Equal(t, "no_mechanism", notices[0].Reason)

	localPresets := []sandboxpolicy.ResolvedAxes{
		{Network: sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.NetworkAllowEntry{{
				Loopback: true,
			}},
		}},
		{Network: sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.NetworkAllowEntry{
				{Domain: "api.anthropic.com", Ports: []int{443}},
				{Domain: "api.openai.com", Ports: []int{443}},
				{Loopback: true},
			},
		}},
	}
	for _, local := range localPresets {
		for _, platform := range []string{"linux", "darwin"} {
			prediction, err = PredictAccessEnforcement(
				openCode, sandboxpolicy.ImplementationTclaudeLayer,
				local, OpenCodeSandboxTclaudeLayer, platform,
			)
			require.NoError(t, err)
			preview = DescribePredictedAccess(local, prediction).Network
			assert.Equal(t, AccessPredictionRefused, preview.Outcome)
			assert.Contains(t, preview.Detail, SandboxCapabilityModelTransport)
			assert.Contains(t, preview.Detail, "no explicit provider")
		}
	}

	portScopedLoopback := sandboxpolicy.ResolvedAxes{
		Network: sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.NetworkAllowEntry{{
				Loopback: true, Ports: []int{11434},
			}},
		},
	}
	prediction, err = PredictAccessEnforcement(
		openCode, sandboxpolicy.ImplementationTclaudeLayer,
		portScopedLoopback, OpenCodeSandboxTclaudeLayer, "linux",
	)
	require.NoError(t, err)
	preview = DescribePredictedAccess(portScopedLoopback, prediction).Network
	assert.Equal(t, AccessPredictionEnforced, preview.Outcome,
		"ordinary explicit-provider loopback lists retain general M3 support")
	assert.NotContains(t, preview.Detail, "no explicit provider")
}

func TestM2cHostDomainEntriesArePreviewedAndLaunchedAsEnforced(t *testing.T) {
	axes := sandboxpolicy.ResolvedAxes{
		Network: sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.NetworkAllowEntry{
				{CIDR: "192.0.2.0/24", Ports: []int{443}},
				{Host: "api.example.test", Ports: []int{443}},
				{Domain: "example.test", IncludeSubdomains: true, Ports: []int{443}},
			},
		},
	}
	prediction, err := PredictAccessEnforcement(
		Default(), sandboxpolicy.ImplementationTclaudeLayer,
		axes, ClaudeSandboxOff, "linux",
	)
	require.NoError(t, err)
	preview := DescribePredictedAccess(axes, prediction).Network
	assert.Equal(t, AccessPredictionEnforced, preview.Outcome)
	assert.Contains(t, preview.Detail, "the network allow list")
	assert.Contains(t, preview.Detail, "outbound traffic is open")

	launch, err := ResolveAccessEnforcement(
		Default(),
		sandboxpolicy.ImplementationTclaudeLayer,
		axes,
		LaunchOSSandbox{
			State:           "on",
			Source:          "test filtered boundary",
			FilteredNetwork: true,
		},
		ClaudeSandboxOff,
	)
	require.NoError(t, err)
	rendered, notices, err := PlanAccessEnforcement(axes, launch)
	if runtime.GOOS == "linux" {
		require.NoError(t, err)
		assert.Equal(t, axes, rendered)
		assert.Empty(t, notices)
		return
	}
	require.NoError(t, err)
	assert.Equal(t, sandboxpolicy.AccessModeOpen, rendered.Network.Mode)
	require.Len(t, notices, 1)
	assert.Equal(t, "no_mechanism", notices[0].Reason)
}

func TestPredictedNetworkEntriesProjectListWideAndPerEntryOutcomes(t *testing.T) {
	rules := sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{
			{CIDR: "192.0.2.0/24", Ports: []int{443}},
			{Domain: "example.test", Ports: []int{443}},
		},
	}
	caps := PredictedAccessEnforcement{
		NetworkList: EnforceFull,
		NetworkSelectors: []NetworkSelectorCapability{
			{Selector: string(sandboxpolicy.NetworkSelectorCIDR), Level: EnforceFull},
			{
				Selector: string(sandboxpolicy.NetworkSelectorDomain),
				Level:    EnforcePartial, Detail: "DNS identity is lease-bound.",
			},
		},
		NetworkPorts:         EnforceFull,
		NetworkListCondition: "Live network probes must pass.",
		Scope:                "process",
		Mechanism:            "test filter",
	}
	rows := DescribePredictedNetworkEntries(rules, caps)
	require.Len(t, rows, 2)
	assert.Equal(t, AccessPredictionEnforced, rows[0].Outcome)
	assert.Equal(t, "allow", rows[0].Mode)
	assert.Equal(t, []string{
		`allow:{"cidr":"192.0.2.0/24","ports":[443]}`,
		`{"cidr":"192.0.2.0/24","ports":[443]}`,
	}, rows[0].Keys)
	assert.Contains(t, rows[0].Detail, "Live network probes must pass")
	assert.Equal(t, AccessPredictionEnforcedPartial, rows[1].Outcome)
	assert.Contains(t, rows[1].Detail, "DNS identity is lease-bound")

	caps.NetworkSelectors = caps.NetworkSelectors[:1]
	rows = DescribePredictedNetworkEntries(rules, caps)
	require.Len(t, rows, 2)
	for _, row := range rows {
		assert.Equal(t, AccessPredictionNotEnforced, row.Outcome,
			"one unsupported selector widens the whole list to open")
		assert.Contains(t, row.Detail, "all outbound connections are permitted")
	}

	caps.NetworkList = EnforceNone
	caps.NetworkListRefusal = "the target refuses this list"
	rows = DescribePredictedNetworkEntries(rules, caps)
	require.Len(t, rows, 2)
	for _, row := range rows {
		assert.Equal(t, AccessPredictionRefused, row.Outcome)
		assert.Equal(t, "the target refuses this list", row.Detail)
	}
}

func TestPredictedNetworkDenyEntriesFollowPolarityCapabilities(t *testing.T) {
	entry := sandboxpolicy.NetworkAllowEntry{
		Domain: "blocked.example", Ports: []int{443},
	}
	rows := DescribePredictedNetworkDenyEntries(
		[]sandboxpolicy.NetworkAllowEntry{entry},
		PredictedAccessEnforcement{},
	)
	require.Len(t, rows, 1)
	assert.Equal(t, "deny", rows[0].Mode)
	assert.Equal(t, AccessPredictionNotEnforced, rows[0].Outcome)
	assert.Equal(t, PredictedNetworkDenyNotEnforcedDetail, rows[0].Detail)
	assert.Equal(t, []string{
		`deny:{"domain":"blocked.example","ports":[443]}`,
		`{"domain":"blocked.example","ports":[443]}`,
	}, rows[0].Keys)

	caps := PredictedAccessEnforcement{
		NetworkDenySelectors: []NetworkSelectorCapability{{
			Selector: string(sandboxpolicy.NetworkSelectorDomain),
			Level:    EnforceFull,
		}},
		NetworkDenyPorts:     EnforceFull,
		NetworkListCondition: "Live network probes must pass.",
		Scope:                "process",
		Mechanism:            "test gateway",
	}
	rows = DescribePredictedNetworkDenyEntries(
		[]sandboxpolicy.NetworkAllowEntry{entry}, caps)
	require.Len(t, rows, 1)
	assert.Equal(t, AccessPredictionEnforced, rows[0].Outcome)
	assert.Contains(t, rows[0].Detail, "Live network probes must pass")

	caps.NetworkDenySelectors[0].Level = EnforcePartial
	caps.NetworkDenySelectors[0].Detail =
		FilteredNetworkDNSDenyDefaultAllowCaveat
	rows = DescribePredictedNetworkDenyEntries(
		[]sandboxpolicy.NetworkAllowEntry{entry}, caps)
	assert.Equal(t, AccessPredictionEnforcedPartial, rows[0].Outcome)
	assert.Contains(t, rows[0].Detail,
		"encrypted DNS that bypasses the broker")

	caps.NetworkDenySelectors[0].Level = EnforceFull
	caps.NetworkDenyPorts = EnforceNone
	rows = DescribePredictedNetworkDenyEntries(
		[]sandboxpolicy.NetworkAllowEntry{entry}, caps)
	assert.Equal(t, AccessPredictionNotEnforced, rows[0].Outcome)
	assert.Contains(t, rows[0].Detail,
		"omitted rather than widened")
}

func TestPlanAccessEnforcementPersistsPerSelectorPartialDetails(t *testing.T) {
	axes := sandboxpolicy.ResolvedAxes{
		Network: sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.NetworkAllowEntry{
				{Host: "api.example.com", Ports: []int{443}},
				{CIDR: "192.0.2.0/24", Ports: []int{443}},
			},
		},
	}
	caps := AccessEnforcement{
		networkList: EnforceFull,
		networkSelectors: []NetworkSelectorCapability{
			{Selector: "host", Level: EnforcePartial, Detail: filteredNetworkDNSCaveat()},
			{Selector: "cidr", Level: EnforceFull},
		},
		networkPorts: EnforceFull,
		scope:        "process",
		mechanism:    "future filtered gateway",
	}
	rendered, notices, err := PlanAccessEnforcement(axes, caps)
	require.NoError(t, err)
	assert.Equal(t, axes, rendered)
	require.Len(t, notices, 1)
	assert.Equal(t, "selector_partial", notices[0].Reason)
	assert.Equal(t, []int{0}, notices[0].Entries)
	assert.Contains(t, notices[0].Detail, FilteredNetworkDNSIdentityCaveat)
}

// TCL-931. A loopback rule carrying a PORT is a strictly tighter policy than
// the bare loopback preset. Before this test's subject, tightening it that way
// made the launch strictly MORE permissive on a platform that cannot enforce
// OpenCode's local list: the bare preset was refused, and the port-scoped form
// fell through both preset recognizers and planned OPEN.
//
// The assertions are deliberately two-directional. Asserting only that darwin
// refuses would pass for a fix that refused every OpenCode filtered list on
// every platform, which would be a Linux regression — Linux enforces this shape
// and must keep planning it as a list.
//
// SCOPE OF WHAT THIS PROVES. Everything here is the capability/plan layer,
// which is fully provable from any host because goos is a PARAMETER to
// accessEnforcementTable rather than runtime.GOOS. It does NOT prove that a
// real Darwin host cannot launch this shape; the launch seam reads
// runtime.GOOS, so that needs a Darwin runner. The refusal reaching a launch
// is not asserted here — it is structural: PlanAccessEnforcement returns the
// refusal as an error, and the guard, the launcher and the watcher all reach
// it through ResolveAccessEnforcement + PlanAccessEnforcement.
func TestOpenCodeLocalOnlyIntentIsRefusedWhereItCannotBeEnforced(t *testing.T) {
	listOf := func(allow ...sandboxpolicy.NetworkAllowEntry) sandboxpolicy.ResolvedAxes {
		return sandboxpolicy.ResolvedAxes{Network: sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeList, Allow: allow,
		}}
	}
	portScoped := listOf(sandboxpolicy.NetworkAllowEntry{
		Loopback: true, Ports: []int{8080},
	})
	// Two loopback rows, so the subject is the local-only FAMILY and not one
	// entry that happens to carry a port.
	repeated := listOf(
		sandboxpolicy.NetworkAllowEntry{Loopback: true, Ports: []int{8080}},
		sandboxpolicy.NetworkAllowEntry{Loopback: true, Ports: []int{9090}},
	)
	plan := func(t *testing.T, h *Harness, axes sandboxpolicy.ResolvedAxes, goos string) (
		accessEnforcementTableRow, error,
	) {
		t.Helper()
		row, err := accessEnforcementTable(
			h, sandboxpolicy.ImplementationTclaudeLayer, axes, ClaudeSandboxOff, goos, true)
		require.NoError(t, err)
		_, _, planErr := PlanAccessEnforcement(axes, accessEnforcementFromTable(row))
		return row, planErr
	}

	// THE DEFECT: darwin OpenCode must refuse rather than widen to open. These
	// shapes do not deploy the proxy, so activating that engine does not change
	// their native-path refusal.
	for _, tc := range []struct {
		name string
		axes sandboxpolicy.ResolvedAxes
	}{
		{"port-scoped", portScoped},
		{"repeated loopback rows", repeated},
	} {
		t.Run("darwin/opencode/"+tc.name, func(t *testing.T) {
			row, planErr := plan(t, MustGet(OpenCodeName), tc.axes, "darwin")
			require.Error(t, planErr,
				"a local-only OpenCode list this platform cannot enforce must be refused, not widened to open")
			// Discriminating, not merely "an error happened": the refusal must
			// be THIS one, and must not offer the explicit-provider remedy,
			// which does not help on this native path.
			assert.Contains(t, planErr.Error(), "unsupported_filtered_network_posture")
			assert.Contains(t, planErr.Error(), "outbound network access fully open")
			assert.NotContains(t, planErr.Error(), "explicit-provider OpenCode config")
			assert.Equal(t, EnforceNone, row.NetworkList)
		})
	}

	// THE BARE PRESET keeps its own, more specific refusal rather than being
	// overwritten by the broader one.
	t.Run("darwin/opencode/bare preset keeps its own message", func(t *testing.T) {
		_, planErr := plan(t, MustGet(OpenCodeName),
			listOf(sandboxpolicy.NetworkAllowEntry{Loopback: true}), "darwin")
		require.Error(t, planErr)
		assert.Contains(t, planErr.Error(), "unsupported_filtered_model_transport")
	})

	// NO LINUX REGRESSION. Linux activates these cells, so the same shapes must
	// still plan as an enforced list. Without this, a fix that refused
	// everywhere would pass the assertions above.
	for _, tc := range []struct {
		name string
		axes sandboxpolicy.ResolvedAxes
	}{
		{"port-scoped", portScoped},
		{"repeated loopback rows", repeated},
	} {
		t.Run("linux/opencode/"+tc.name, func(t *testing.T) {
			row, planErr := plan(t, MustGet(OpenCodeName), tc.axes, "linux")
			require.NoError(t, planErr,
				"Linux enforces this shape; refusing it here would remove a working explicit-provider configuration")
			assert.Equal(t, EnforceFull, row.NetworkList)
		})
	}

	// THE OTHER HARNESSES are untouched on the same platform and shape, which
	// is what makes this a harness-specific gap rather than a platform rule.
	for _, name := range []string{DefaultName, CodexName} {
		t.Run("darwin/"+name+"/port-scoped stays enforced", func(t *testing.T) {
			row, planErr := plan(t, MustGet(name), portScoped, "darwin")
			require.NoError(t, planErr)
			assert.Equal(t, EnforcePartial, row.NetworkList,
				"TCL-927 rates this Partial; it must not become a refusal or Full")
		})
	}

	// Every harness activates this general proxy shape on Darwin, with OpenCode
	// backed by its distinct agentd-owned server smoke.
	t.Run("darwin/non-local-intent activation follows evidence", func(t *testing.T) {
		domainOnly := listOf(sandboxpolicy.NetworkAllowEntry{
			Domain: "api.example.com", Ports: []int{443},
		})
		domainOnly.Network.Engine = sandboxpolicy.NetworkEngineProxy
		for _, name := range []string{DefaultName, CodexName} {
			row, planErr := plan(t, MustGet(name), domainOnly, "darwin")
			require.NoErrorf(t, planErr, "harness %s", name)
			assert.Equalf(t, EnforcePartial, row.NetworkList,
				"harness %s must retain the TCL-917 Darwin cap", name)
		}
		row, planErr := plan(t, MustGet(OpenCodeName), domainOnly, "darwin")
		require.NoError(t, planErr)
		assert.Equal(t, EnforcePartial, row.NetworkList)
		assert.Contains(t, row.NetworkListCondition,
			OpenCodeFilteredExplicitProviderCaveat)
	})

	// The predicate answers the INTENT question, not the recognizer one. A
	// port-scoped loopback list is local-only intent while being neither preset.
	t.Run("predicate separates intent from recognition", func(t *testing.T) {
		assert.True(t, IsLocalOnlyNetworkIntent(portScoped.Network))
		assert.False(t, IsLocalAccessNetworkPreset(portScoped.Network))
		assert.False(t, IsLocalModelAPIsNetworkPreset(portScoped.Network))
	})
}

func TestDarwinTclaudeLayerEnforcesOnlyLoopbackOnlyLists(t *testing.T) {
	local := sandboxpolicy.ResolvedAxes{Network: sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{{
			Loopback: true, Ports: []int{11434},
		}},
	}}
	row, err := accessEnforcementTable(
		Default(), sandboxpolicy.ImplementationTclaudeLayer,
		local, ClaudeSandboxOff, "darwin", true,
	)
	require.NoError(t, err)
	// TCL-927: Partial, not Full. This row shipped at Full from TCL-833 and
	// claimed a loopback confinement Seatbelt cannot express — TCL-917
	// measured a service on an authored port at a non-loopback address of the
	// same machine reachable from inside the sandbox. Asserting the exact
	// level rather than "not None" is what keeps a silent return to Full from
	// passing here.
	assert.Equal(t, EnforcePartial, row.NetworkList)
	// The PORTS axis is genuinely enforced (off-list port refused with EPERM,
	// measured), so this stays Full. Pinned so a future correction of the
	// address axis does not quietly take the port claim down with it.
	assert.Equal(t, EnforceFull, row.NetworkPorts)
	assert.Equal(t, []NetworkSelectorCapability{{
		Selector: string(sandboxpolicy.NetworkSelectorLoopback),
		Level:    EnforcePartial,
		Detail:   SeatbeltNativeLoopbackSelectorDetail,
	}}, row.NetworkSelectors)
	// The escape must reach an operator who reads the ROW without opening the
	// selector, which is the whole reason the condition is populated.
	//
	// Asserting equality against the constant would pin the WIRING and nothing
	// else: it passes for any body, so deleting the load-bearing clause would
	// regress the disclosure with this test still green. The clause itself is
	// asserted too.
	assert.Equal(t, SeatbeltNativeLoopbackCondition, row.NetworkListCondition)
	assert.Contains(t, row.NetworkListCondition, "not confined to loopback")
	assert.Contains(t, row.NetworkSelectors[0].Detail, "not confined to loopback")

	// The branch covers Codex as well as Claude Code, and a gate that silently
	// stopped covering one of them would otherwise pass here.
	codexRow, err := accessEnforcementTable(
		MustGet(CodexName), sandboxpolicy.ImplementationTclaudeLayer,
		local, ClaudeSandboxOff, "darwin", true,
	)
	require.NoError(t, err)
	assert.Equal(t, EnforcePartial, codexRow.NetworkList)
	assert.Equal(t, row.NetworkSelectors, codexRow.NetworkSelectors)
	assert.Equal(t, row.NetworkListCondition, codexRow.NetworkListCondition)
	rendered, notices, err := PlanAccessEnforcement(
		local, accessEnforcementFromTable(row))
	require.NoError(t, err)
	assert.Equal(t, local, rendered)
	// The plan is still rendered as authored — Partial is a disclosure change,
	// not a narrowing — but it now carries a degradation notice, because the
	// enforced set really is wider than what was authored. Asserted positively:
	// the previous version of this test asserted the notice list was EMPTY,
	// which is a condition an absent notice satisfies just as well as a
	// correct one.
	require.Len(t, notices, 1)
	assert.Equal(t, "selector_partial", notices[0].Reason)
	assert.Equal(t, []int{0}, notices[0].Entries)
	assert.Contains(t, notices[0].Detail, SeatbeltNativeLoopbackSelectorDetail)

	mixed := sandboxpolicy.ResolvedAxes{Network: sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{
			{Loopback: true},
			{Domain: "api.example.com", Ports: []int{443}},
		},
	}}
	row, err = accessEnforcementTable(
		Default(), sandboxpolicy.ImplementationTclaudeLayer,
		mixed, ClaudeSandboxOff, "darwin", false,
	)
	require.NoError(t, err)
	assert.Equal(t, EnforceNone, row.NetworkList,
		"existing mixed Darwin lists must keep their historical NotEnforced result")
	assert.Empty(t, row.NetworkListRefusal)
	assert.Empty(t, row.NetworkSelectorRefusal)
	rendered, notices, err = PlanAccessEnforcement(
		mixed, accessEnforcementFromTable(row))
	require.NoError(t, err)
	assert.Equal(t, sandboxpolicy.AccessModeOpen, rendered.Network.Mode)
	require.Len(t, notices, 1)
	assert.Equal(t, "no_mechanism", notices[0].Reason)
	assert.Equal(t, sandboxpolicy.AccessNoticeEffectNotEnforced, notices[0].Effect)
	assert.Contains(t, notices[0].Detail, "outbound network access remains open")
}

func TestClosedPartialEnforcesAndWarnsButClosedNoneRefuses(t *testing.T) {
	axes := sandboxpolicy.ResolvedAxes{
		UnixSockets: sandboxpolicy.UnixSocketRules{Mode: sandboxpolicy.AccessModeClosed},
	}
	partial := AccessEnforcement{
		socketClosed: EnforcePartial, scope: "process", mechanism: "future Linux socket wall",
	}
	rendered, notices, err := PlanAccessEnforcement(axes, partial)
	require.NoError(t, err)
	assert.Equal(t, sandboxpolicy.AccessModeClosed, rendered.UnixSockets.Mode)
	require.Len(t, notices, 1)
	assert.Equal(t, sandboxpolicy.AccessNoticeEffectEnforcedWider, notices[0].Effect)

	none := partial
	none.socketClosed = EnforceNone
	_, _, err = PlanAccessEnforcement(axes, none)
	require.ErrorContains(t, err, "cannot enforce closed Unix-socket access")
}

func TestClosedNetworkOverrideWidensOnlyThatAxisAndPinsRefusalCopy(t *testing.T) {
	axes := sandboxpolicy.ResolvedAxes{
		Network:     sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeClosed},
		UnixSockets: sandboxpolicy.UnixSocketRules{Mode: sandboxpolicy.AccessModeClosed},
	}
	caps := AccessEnforcement{
		networkClosed: EnforceNone,
		socketClosed:  EnforceFull,
		scope:         "tools-only",
		mechanism:     "Codex builtin sandbox",
	}
	const refusal = "Codex builtin sandbox (tools-only scope) cannot enforce closed network access; " +
		"choose a sandbox implementation that can enforce closed network access, use network open, " +
		"or enable “Allow launch without enforcement” in the dashboard spawn dialog"

	_, _, err := PlanAccessEnforcement(axes, caps)
	var capability *SandboxCapabilityError
	require.ErrorAs(t, err, &capability)
	assert.Equal(t, SandboxCapabilityNetworkAllowlist, capability.Kind)
	assert.Equal(t, refusal, capability.Message)

	predicted := DescribePredictedAccess(axes, PredictedAccessEnforcement{
		NetworkClosed: EnforceNone,
		SocketClosed:  EnforceFull,
		Scope:         "tools-only",
		Mechanism:     "Codex builtin sandbox",
	})
	assert.Equal(t, AccessPredictionRefused, predicted.Network.Outcome)
	assert.Equal(t, refusal, predicted.Network.Detail)

	rendered, notices, err := PlanAccessEnforcement(
		axes, caps, AccessEnforcementOptions{AllowUnenforcedNetworkClosed: true})
	require.NoError(t, err)
	assert.Equal(t, sandboxpolicy.AccessModeOpen, rendered.Network.Mode)
	assert.Equal(t, sandboxpolicy.AccessModeClosed, rendered.UnixSockets.Mode,
		"the override must not drop an enforceable independent axis")
	require.Len(t, notices, 1)
	assert.Equal(t, sandboxpolicy.AccessNoticeClassDegradation, notices[0].Class)
	assert.Equal(t, "network", notices[0].Axis)
	assert.Equal(t, sandboxpolicy.AccessNoticeReasonOperatorUnenforcedLaunchOverride,
		notices[0].Reason)
	assert.Equal(t, sandboxpolicy.AccessNoticeEffectNotEnforced, notices[0].Effect)
	assert.Contains(t, notices[0].Detail, "human operator used the dashboard launch override")
	assert.Contains(t, notices[0].Detail, "outbound network access remains open")

	caps.socketClosed = EnforceNone
	_, _, err = PlanAccessEnforcement(
		axes, caps, AccessEnforcementOptions{AllowUnenforcedNetworkClosed: true})
	require.ErrorContains(t, err, "cannot enforce closed Unix-socket access",
		"the network-only option must not become a generic capability bypass")
}

// TestComposedNetworkDisclosureSentencesAreSeparated pins TCL-864's copy
// contract: the baseline verdict, the launch-check condition and each deny
// row's limitation are authored independently, so the composed disclosure must
// join them as separate sentences instead of running them together. Every
// clause must still survive verbatim.
func TestComposedNetworkDisclosureSentencesAreSeparated(t *testing.T) {
	deny := sandboxpolicy.NetworkAllowEntry{Domain: "telemetry.example", Ports: []int{443}}
	axes := sandboxpolicy.ResolvedAxes{
		Network: sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeOpen,
			Deny: []sandboxpolicy.NetworkAllowEntry{deny},
		},
	}

	// A target that applies no deny entries: the ambient-access baseline is
	// followed by the whole fail-open deny limitation.
	predicted := DescribePredictedAccess(axes, PredictedAccessEnforcement{
		Mechanism: "test sandbox",
		Scope:     "process",
	})
	assert.Equal(t, AccessPredictionEnforcedPartial, predicted.Network.Outcome)
	assert.Equal(t,
		"ambient outbound network access remains available. Deny limitation: "+
			PredictedNetworkDenyNotEnforcedDetail,
		predicted.Network.Detail)

	// A target that does apply them keeps the same separation on the
	// enforced-deny sentence.
	predicted = DescribePredictedAccess(axes, PredictedAccessEnforcement{
		NetworkDenySelectors: []NetworkSelectorCapability{{
			Selector: string(sandboxpolicy.NetworkSelectorDomain),
			Level:    EnforceFull,
		}},
		NetworkDenyPorts: EnforceFull,
		Mechanism:        "test sandbox",
		Scope:            "process",
	})
	assert.Equal(t,
		"ambient outbound network access remains available. Configured deny rules are enforced.",
		predicted.Network.Detail)

	// Allow rows compose the launch-check condition the same way as deny rows.
	allowRows := DescribePredictedNetworkEntries(
		sandboxpolicy.NetworkRules{
			Mode:  sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.NetworkAllowEntry{{Domain: "api.example.com", Ports: []int{443}}},
		},
		PredictedAccessEnforcement{
			NetworkList: EnforceFull,
			NetworkSelectors: []NetworkSelectorCapability{{
				Selector: string(sandboxpolicy.NetworkSelectorDomain),
				Level:    EnforceFull,
			}},
			NetworkPorts:         EnforceFull,
			NetworkListCondition: "Live network probes must pass.",
			Mechanism:            "test gateway",
			Scope:                "process",
		})
	require.Len(t, allowRows, 1)
	assert.Equal(t,
		"test gateway enforces this destination. Live network probes must pass.",
		allowRows[0].Detail)

	// Per-row details compose the launch-check condition the same way.
	rows := DescribePredictedNetworkDenyEntries(
		[]sandboxpolicy.NetworkAllowEntry{deny},
		PredictedAccessEnforcement{
			NetworkDenySelectors: []NetworkSelectorCapability{{
				Selector: string(sandboxpolicy.NetworkSelectorDomain),
				Level:    EnforceFull,
			}},
			NetworkDenyPorts:     EnforceFull,
			NetworkListCondition: "Live network probes must pass.",
			Mechanism:            "test gateway",
			Scope:                "process",
		})
	require.Len(t, rows, 1)
	assert.Equal(t,
		"test gateway enforces this deny destination. Live network probes must pass.",
		rows[0].Detail)
}

// TestAppendPredictionSentencePreservesFinalCharacter pins the invariant the
// separator fix rests on: appendPredictionSentence only ever inserts a
// terminator BETWEEN two clauses, so a composed detail always ends exactly as
// its last clause ended. Downstream summaries join whole details with their
// own separator (sandbox_profile_prediction.go), so a helper that terminated
// the tail would silently change how every one of those lines reads.
func TestAppendPredictionSentencePreservesFinalCharacter(t *testing.T) {
	clauses := []string{
		"", " ", "ambient outbound network access remains available",
		"Deny limitation: this rule is saved",
		"Live network probes must pass.",
		"is the boundary enforced?", "traffic is open!",
		"the mechanism widened the list; disclosed traffic remains reachable",
		PredictedNetworkDenyNotEnforcedDetail,
		FilteredNetworkDNSDenyDefaultAllowCaveat,
	}
	for _, base := range clauses {
		for _, next := range clauses {
			composed := appendPredictionSentence(base, next)
			trimmedBase, trimmedNext := strings.TrimSpace(base), strings.TrimSpace(next)
			switch {
			case trimmedNext != "":
				assert.True(t, strings.HasSuffix(composed, trimmedNext),
					"composing %q onto %q must end with the appended clause", next, base)
			case trimmedBase != "":
				assert.True(t, strings.HasSuffix(composed, trimmedBase),
					"an empty addition must leave %q's ending alone", base)
			default:
				assert.Equal(t, "", composed)
			}
			// Both clauses must survive whole; separation never rewrites them.
			assert.Contains(t, composed, trimmedBase)
			assert.Contains(t, composed, trimmedNext)
		}
	}

	// The fold inherits the invariant from the pairwise operation.
	assert.True(t, strings.HasSuffix(
		joinPredictionSentences([]string{
			"first clause", "second clause without a stop",
		}), "second clause without a stop"))
}

// A cgroup enforces nothing on any access axis, so a resource-only prediction
// must credit exactly as little as off does. The mechanism string is the one
// place the two differ, and it has to differ: the disclosure names what is
// actually running for this launch, and a CPU/memory cgroup is running.
func TestResourceOnlyPredictionCreditsNoAccessEnforcement(t *testing.T) {
	axes := sandboxpolicy.ResolvedAxes{
		Network: sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeClosed},
		UnixSockets: sandboxpolicy.UnixSocketRules{
			Mode: sandboxpolicy.AccessModeClosed,
		},
	}
	for _, harnessName := range []string{DefaultName, CodexName, OpenCodeName} {
		t.Run(harnessName, func(t *testing.T) {
			resourceOnly, err := PredictAccessEnforcement(
				MustGet(harnessName), sandboxpolicy.ImplementationResourceOnly,
				axes, "", "linux",
			)
			require.NoError(t, err)
			off, err := PredictAccessEnforcement(
				MustGet(harnessName), sandboxpolicy.ImplementationOff,
				axes, "", "linux",
			)
			require.NoError(t, err)

			assert.Equal(t, EnforceNone, resourceOnly.NetworkClosed)
			assert.Equal(t, EnforceNone, resourceOnly.NetworkList)
			assert.Equal(t, EnforceNone, resourceOnly.SocketClosed)
			assert.Equal(t, EnforceNone, resourceOnly.SocketList)
			assert.Equal(t, off.NetworkClosed, resourceOnly.NetworkClosed)
			assert.Equal(t, off.SocketClosed, resourceOnly.SocketClosed)

			assert.Equal(t, "sandbox off; cgroup resource limits only",
				resourceOnly.Mechanism,
				"the mechanism must say both halves: no confinement, and a real cgroup")
			assert.NotEqual(t, off.Mechanism, resourceOnly.Mechanism,
				"a limits-only launch must not be disclosed as a plain sandbox-off launch")
		})
	}
}

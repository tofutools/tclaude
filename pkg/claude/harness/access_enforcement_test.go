package harness

import (
	"runtime"
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

	hostOpenClosedSockets := sandboxpolicy.ResolvedAxes{
		Network:     sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeOpen},
		UnixSockets: sandboxpolicy.UnixSocketRules{Mode: sandboxpolicy.AccessModeClosed},
	}
	caps, err = accessEnforcementForTargetForTest(
		h, sandboxpolicy.ImplementationTclaudeLayer, hostOpenClosedSockets, ClaudeSandboxOff, "linux",
	)
	require.NoError(t, err)
	assert.Equal(t, EnforceNone, caps.socketClosed,
		"removing combination awareness and claiming static Full must fail this assertion")
	_, _, err = PlanAccessEnforcement(hostOpenClosedSockets, caps)
	require.EqualError(t, err,
		`unix_sockets "closed" cannot be enforced with open network access on Linux tclaude-layer; `+
			`close network access as well, use an access list, or run without the socket restriction`)

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
	rendered, notices, err = PlanAccessEnforcement(hostOpenSocketList, caps)
	require.NoError(t, err)
	assert.Equal(t, sandboxpolicy.AccessModeOpen, rendered.UnixSockets.Mode)
	require.Len(t, notices, 1)
	assert.Contains(t, notices[0].Detail,
		"unenforced under host-open network on Linux tclaude-layer")
	assert.Contains(t, notices[0].Detail, "host-open",
		"a delivered widening must disclose the same socket-open surface the renderer receives")

	// The public resolver consumes the same axes rather than returning a static
	// per-target capability descriptor.
	resolved, err := ResolveAccessEnforcement(
		h, sandboxpolicy.ImplementationTclaudeLayer, hostOpenSocketList,
		evidence, ClaudeSandboxOff,
	)
	require.NoError(t, err)
	assert.Equal(t, EnforceNone, resolved.socketClosed)
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
			assert.Contains(t, preview.Detail, "TCL-826")
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
	assert.NotContains(t, preview.Detail, "TCL-826")
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

func TestPredictedNetworkDenyEntriesAreAlwaysHonest(t *testing.T) {
	entry := sandboxpolicy.NetworkAllowEntry{
		Domain: "blocked.example", Ports: []int{443},
	}
	rows := DescribePredictedNetworkDenyEntries([]sandboxpolicy.NetworkAllowEntry{entry})
	require.Len(t, rows, 1)
	assert.Equal(t, "deny", rows[0].Mode)
	assert.Equal(t, AccessPredictionNotEnforced, rows[0].Outcome)
	assert.Equal(t, PredictedNetworkDenyNotEnforcedDetail, rows[0].Detail)
	assert.Equal(t, []string{
		`deny:{"domain":"blocked.example","ports":[443]}`,
		`{"domain":"blocked.example","ports":[443]}`,
	}, rows[0].Keys)
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
	assert.Equal(t, EnforceFull, row.NetworkList)
	assert.Equal(t, EnforceFull, row.NetworkPorts)
	assert.Equal(t, []NetworkSelectorCapability{{
		Selector: string(sandboxpolicy.NetworkSelectorLoopback),
		Level:    EnforceFull,
	}}, row.NetworkSelectors)
	rendered, notices, err := PlanAccessEnforcement(
		local, accessEnforcementFromTable(row))
	require.NoError(t, err)
	assert.Equal(t, local, rendered)
	assert.Empty(t, notices)

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

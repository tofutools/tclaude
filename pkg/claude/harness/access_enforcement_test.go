package harness

import (
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
	assert.Equal(t, EnforceFull, caps.socketClosed)
	_, notices, err := PlanAccessEnforcement(closed, caps)
	require.NoError(t, err)
	assert.Empty(t, notices)

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
	rendered, notices, err := PlanAccessEnforcement(hostOpenSocketList, caps)
	require.NoError(t, err)
	assert.Equal(t, sandboxpolicy.AccessModeOpen, rendered.UnixSockets.Mode)
	require.Len(t, notices, 1)
	assert.Contains(t, notices[0].Detail,
		"unenforced under host-open network on Linux tclaude-layer")

	// The public resolver consumes the same axes rather than returning a static
	// per-target capability descriptor.
	resolved, err := ResolveAccessEnforcement(
		h, sandboxpolicy.ImplementationTclaudeLayer, hostOpenSocketList,
		evidence, ClaudeSandboxOff,
	)
	require.NoError(t, err)
	assert.Equal(t, EnforceNone, resolved.socketClosed)
}

func accessEnforcementForTargetForTest(
	h *Harness,
	implementation sandboxpolicy.Implementation,
	axes sandboxpolicy.ResolvedAxes,
	validatedBuiltinMode, goos string,
) (AccessEnforcement, error) {
	row, err := accessEnforcementTable(h, implementation, axes, validatedBuiltinMode, goos)
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
		networkList: EnforcePartial, networkSelectors: []string{"host"},
		networkPorts: false, scope: "tools-only",
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

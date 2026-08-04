package harness

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// The single-wall implementation must force the SAME harness-native posture for
// every harness, because the mode it forces is the mode tclaude persists and
// every later reader — lineage guard, relaunch replay, simulator — has to agree
// with the launch about what ran (TCL-989).
func TestResolveSandboxImplementationModeForcesTclaudeLayerModeForEveryHarness(t *testing.T) {
	for _, tc := range []struct {
		harness   string
		requested string
		want      string
	}{
		{DefaultName, ClaudeSandboxInherit, ClaudeSandboxOff},
		{DefaultName, ClaudeSandboxOn, ClaudeSandboxOff},
		{DefaultName, "", ClaudeSandboxOff},
		{CodexName, SandboxManagedProfile, SandboxDangerFull},
		{CodexName, SandboxReadOnly, SandboxDangerFull},
		{OpenCodeName, OpenCodeSandboxAccessControl, OpenCodeSandboxTclaudeLayer},
		{OpenCodeName, OpenCodeSandboxTclaudeLayer, OpenCodeSandboxTclaudeLayer},
		{CopilotName, CopilotSandboxInherit, CopilotSandboxOff},
		{CopilotName, "", CopilotSandboxOff},
	} {
		h, err := Resolve(tc.harness)
		require.NoError(t, err)
		got, err := ResolveSandboxImplementationMode(
			h, tc.requested, sandboxpolicy.ImplementationTclaudeLayer)
		require.NoErrorf(t, err, "%s/%s", tc.harness, tc.requested)
		require.Equalf(t, tc.want, got, "%s/%s", tc.harness, tc.requested)
		require.Equalf(t, tc.want, mustTclaudeLayerMode(t, h),
			"the forced mode must be the descriptor's declared single-wall posture")
	}
}

func mustTclaudeLayerMode(t *testing.T, h *Harness) string {
	t.Helper()
	mode, err := TclaudeLayerHarnessBuiltinMode(h)
	require.NoError(t, err)
	return mode
}

// Forcing must be idempotent: the daemon hands the already-forced mode to the
// launch boundary, which resolves it again.
func TestResolveSandboxImplementationModeIsIdempotent(t *testing.T) {
	for _, name := range []string{DefaultName, CodexName, OpenCodeName, CopilotName} {
		h, err := Resolve(name)
		require.NoError(t, err)
		once, err := ResolveSandboxImplementationMode(
			h, "", sandboxpolicy.ImplementationTclaudeLayer)
		require.NoError(t, err)
		twice, err := ResolveSandboxImplementationMode(
			h, once, sandboxpolicy.ImplementationTclaudeLayer)
		require.NoErrorf(t, err, "harness %s", name)
		require.Equalf(t, once, twice, "harness %s", name)
	}
}

// OpenCode's mode axis names the same topology, so its explicit incompatibility
// must survive the generalization rather than being silently forced into
// agreement.
func TestResolveSandboxImplementationModeKeepsOpenCodeConflicts(t *testing.T) {
	h, err := Resolve(OpenCodeName)
	require.NoError(t, err)

	_, err = ResolveSandboxImplementationMode(
		h, OpenCodeSandboxOff, sandboxpolicy.ImplementationTclaudeLayer)
	require.Error(t, err, "off + tclaude-layer must stay incompatible")

	_, err = ResolveSandboxImplementationMode(
		h, OpenCodeSandboxTclaudeLayer, sandboxpolicy.ImplementationHarnessBuiltin)
	require.Error(t, err, "the tclaude-layer MODE must still require the implementation")
}

// stacked runs the harness's own sandbox nested inside tclaude's, so its mode
// still means what it says and must not be reinterpreted as the single wall.
func TestResolveSandboxImplementationModeLeavesNonTclaudeLayerAlone(t *testing.T) {
	for _, implementation := range []sandboxpolicy.Implementation{
		sandboxpolicy.ImplementationHarnessBuiltin,
		sandboxpolicy.ImplementationStacked,
	} {
		for _, tc := range []struct{ harness, mode string }{
			{DefaultName, ClaudeSandboxOn},
			{DefaultName, ClaudeSandboxInherit},
			{CodexName, SandboxManagedProfile},
			{CopilotName, CopilotSandboxInherit},
		} {
			h, err := Resolve(tc.harness)
			require.NoError(t, err)
			got, err := ResolveSandboxImplementationMode(h, tc.mode, implementation)
			require.NoErrorf(t, err, "%s/%s/%s", implementation, tc.harness, tc.mode)
			require.Equalf(t, tc.mode, got, "%s/%s", implementation, tc.harness)
		}
	}
}

// The native resolver is what the daemon's mode-keyed gates judge. It must NOT
// pick up the single-wall forcing, or a tclaude-layer launch would read as an
// unconfined agent to every capability, cwd-conflict and Git-write-path gate.
func TestResolveNativeHarnessBuiltinModeNeverForcesTheSingleWallMode(t *testing.T) {
	for _, tc := range []struct{ harness, mode string }{
		{DefaultName, ClaudeSandboxInherit},
		{DefaultName, ClaudeSandboxOn},
		{CodexName, SandboxManagedProfile},
		{CopilotName, CopilotSandboxInherit},
	} {
		h, err := Resolve(tc.harness)
		require.NoError(t, err)
		got, err := ResolveNativeHarnessBuiltinMode(
			h, tc.mode, sandboxpolicy.ImplementationTclaudeLayer)
		require.NoErrorf(t, err, "%s/%s", tc.harness, tc.mode)
		require.Equalf(t, tc.mode, got, "%s/%s", tc.harness, tc.mode)
	}

	// OpenCode is the one harness whose native mode DOES change: its mode axis
	// names the topology, and the daemon gates have always seen that spelling.
	h, err := Resolve(OpenCodeName)
	require.NoError(t, err)
	got, err := ResolveNativeHarnessBuiltinMode(
		h, OpenCodeSandboxAccessControl, sandboxpolicy.ImplementationTclaudeLayer)
	require.NoError(t, err)
	require.Equal(t, OpenCodeSandboxTclaudeLayer, got)
}

// An explicit implementation=off resolves to each harness's no-confinement mode
// on BOTH entry points; the generalization must not have moved that.
func TestSandboxImplementationOffResolvesTheHarnessOffMode(t *testing.T) {
	for _, tc := range []struct{ harness, want string }{
		{DefaultName, ClaudeSandboxOff},
		{CodexName, SandboxDangerFull},
		{OpenCodeName, OpenCodeSandboxOff},
		{CopilotName, CopilotSandboxOff},
	} {
		h, err := Resolve(tc.harness)
		require.NoError(t, err)
		got, err := ResolveSandboxImplementationMode(
			h, "", sandboxpolicy.ImplementationOff)
		require.NoErrorf(t, err, "harness %s", tc.harness)
		require.Equalf(t, tc.want, got, "harness %s", tc.harness)
		native, err := ResolveNativeHarnessBuiltinMode(
			h, "", sandboxpolicy.ImplementationOff)
		require.NoErrorf(t, err, "harness %s", tc.harness)
		require.Equalf(t, tc.want, native, "harness %s", tc.harness)
	}
}

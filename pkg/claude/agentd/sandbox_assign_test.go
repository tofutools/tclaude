package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// A reassignment must be able to put an agent back on the implementation it was
// born with. relaunchProfileForSpawn records a blank spawn as harness-builtin for
// every harness, so for OpenCode and Copilot — which own no OS sandbox — that IS
// the recorded value; refusing it here would make moving such an agent to
// resource-only a one-way door, with `off` no substitute (it also drops
// OpenCode's own command filter).
func TestValidateAssignedSandboxImplementationAllowsTheHarnessBuiltinDefault(t *testing.T) {
	for _, name := range []string{harness.OpenCodeName, harness.CopilotName} {
		t.Run(name, func(t *testing.T) {
			h, err := harness.Resolve(name)
			require.NoError(t, err)
			require.False(t, h.SupportsBuiltinOSSandbox(),
				"this test is only meaningful for a harness that owns no OS sandbox")

			_, pinErr := validateSandboxImplementationForHarness(
				h, string(sandboxpolicy.ImplementationHarnessBuiltin))
			require.Error(t, pinErr,
				"authoring a profile that PINS harness-builtin still asserts confinement "+
					"this harness does not provide")

			got, err := validateAssignedSandboxImplementation(
				h, string(sandboxpolicy.ImplementationHarnessBuiltin))
			require.NoError(t, err, "restoring the recorded default is not that assertion")
			assert.Equal(t, string(sandboxpolicy.ImplementationHarnessBuiltin), got)
		})
	}
}

// The capability gates that describe what tclaude will actually run must survive
// the relaxation above.
func TestValidateAssignedSandboxImplementationStillGatesCapabilities(t *testing.T) {
	h, err := harness.Resolve(harness.OpenCodeName)
	require.NoError(t, err)
	_, err = validateAssignedSandboxImplementation(
		h, string(sandboxpolicy.ImplementationStacked))
	assert.Error(t, err, "OpenCode owns no nested sandbox to stack under the outer layer")

	_, err = validateAssignedSandboxImplementation(h, "resource_only")
	assert.Error(t, err, "an unknown value is still invalid")
}

// The predicate that decides whether a recorded mode is worth carrying forward
// must name exactly the implementations that DERIVE the launch mode — the two
// forcing branches in harness.ResolveSandboxImplementationMode. Adding a value
// here that does not force, or omitting one that does, silently changes what
// "restore the default" records.
func TestSandboxImplementationForcesLaunchMode(t *testing.T) {
	forcing := map[sandboxpolicy.Implementation]bool{
		sandboxpolicy.ImplementationOff:            true,
		sandboxpolicy.ImplementationResourceOnly:   true,
		sandboxpolicy.ImplementationTclaudeLayer:   true,
		sandboxpolicy.ImplementationHarnessBuiltin: false,
		sandboxpolicy.ImplementationStacked:        false,
	}
	claude, err := harness.Resolve(harness.DefaultName)
	require.NoError(t, err)
	for implementation, want := range forcing {
		assert.Equal(t, want, sandboxImplementationForcesLaunchMode(implementation),
			"implementation %s", implementation)
		if !want {
			continue
		}
		// Cross-check against the resolver itself: a forcing implementation
		// discards the requested mode, which is why carrying it forward is wrong.
		resolved, resolveErr := harness.ResolveSandboxImplementationMode(
			claude, harness.ClaudeSandboxOn, implementation)
		require.NoError(t, resolveErr)
		assert.NotEqual(t, harness.ClaudeSandboxOn, resolved,
			"implementation %s was expected to force its own mode", implementation)
	}
}

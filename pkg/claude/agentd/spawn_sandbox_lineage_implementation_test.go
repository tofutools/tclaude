package agentd

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// lineageMatrixCase is one admission decision of the pre-TCL-989 matrix. The
// whole set is replayed below with the implementation field blank (a legacy
// row), explicitly harness-builtin, and explicitly stacked — none of which may
// change a single verdict.
type lineageMatrixCase struct {
	parentHarness string
	parentMode    string
	childHarness  string
	childMode     string
	allowed       bool
}

func sandboxLineageMatrix() []lineageMatrixCase {
	return []lineageMatrixCase{
		// Claude parents.
		{harness.DefaultName, harness.ClaudeSandboxOff, harness.DefaultName, harness.ClaudeSandboxOff, true},
		{harness.DefaultName, harness.ClaudeSandboxOff, harness.CodexName, harness.SandboxDangerFull, true},
		{harness.DefaultName, harness.ClaudeSandboxOff, harness.OpenCodeName, harness.OpenCodeSandboxOff, true},
		{harness.DefaultName, harness.ClaudeSandboxInherit, harness.DefaultName, harness.ClaudeSandboxInherit, true},
		{harness.DefaultName, harness.ClaudeSandboxInherit, harness.DefaultName, harness.ClaudeSandboxOn, true},
		{harness.DefaultName, harness.ClaudeSandboxInherit, harness.DefaultName, harness.ClaudeSandboxOff, false},
		{harness.DefaultName, harness.ClaudeSandboxInherit, harness.CodexName, harness.SandboxManagedProfile, true},
		{harness.DefaultName, harness.ClaudeSandboxInherit, harness.CodexName, harness.SandboxDangerFull, false},
		{harness.DefaultName, harness.ClaudeSandboxInherit, harness.OpenCodeName, harness.OpenCodeSandboxTclaudeLayer, false},
		{harness.DefaultName, harness.ClaudeSandboxOn, harness.DefaultName, harness.ClaudeSandboxOn, true},
		{harness.DefaultName, harness.ClaudeSandboxOn, harness.DefaultName, harness.ClaudeSandboxInherit, false},
		{harness.DefaultName, harness.ClaudeSandboxOn, harness.CodexName, harness.SandboxReadOnly, true},

		// Codex parents.
		{harness.CodexName, harness.SandboxDangerFull, harness.DefaultName, harness.ClaudeSandboxOff, true},
		{harness.CodexName, harness.SandboxManagedProfile, harness.CodexName, harness.SandboxManagedProfile, true},
		{harness.CodexName, harness.SandboxManagedProfile, harness.CodexName, harness.SandboxDangerFull, false},
		{harness.CodexName, harness.SandboxManagedProfile, harness.DefaultName, harness.ClaudeSandboxOn, true},
		{harness.CodexName, harness.SandboxManagedProfile, harness.DefaultName, harness.ClaudeSandboxOff, false},
		{harness.CodexName, harness.SandboxWorkspaceWrite, harness.CodexName, harness.SandboxWorkspaceWrite, true},
		{harness.CodexName, harness.SandboxWorkspaceWrite, harness.CodexName, harness.SandboxManagedProfile, false},
		{harness.CodexName, harness.SandboxWorkspaceWrite, harness.DefaultName, harness.ClaudeSandboxOn, false},
		{harness.CodexName, harness.SandboxReadOnly, harness.CodexName, harness.SandboxReadOnly, true},
		{harness.CodexName, harness.SandboxReadOnly, harness.CodexName, harness.SandboxWorkspaceWrite, false},

		// OpenCode parents.
		{harness.OpenCodeName, harness.OpenCodeSandboxOff, harness.DefaultName, harness.ClaudeSandboxOff, true},
		{harness.OpenCodeName, harness.OpenCodeSandboxTclaudeLayer, harness.OpenCodeName, harness.OpenCodeSandboxTclaudeLayer, true},
		{harness.OpenCodeName, harness.OpenCodeSandboxTclaudeLayer, harness.OpenCodeName, harness.OpenCodeSandboxAccessControl, false},
		{harness.OpenCodeName, harness.OpenCodeSandboxTclaudeLayer, harness.DefaultName, harness.ClaudeSandboxOn, true},
		{harness.OpenCodeName, harness.OpenCodeSandboxAccessControl, harness.OpenCodeName, harness.OpenCodeSandboxAccessControl, true},
		{harness.OpenCodeName, harness.OpenCodeSandboxAccessControl, harness.OpenCodeName, harness.OpenCodeSandboxTclaudeLayer, true},
		{harness.OpenCodeName, harness.OpenCodeSandboxAccessControl, harness.CodexName, harness.SandboxManagedProfile, true},

		// Copilot is not admitted on either side, in any implementation.
		{harness.CopilotName, harness.CopilotSandboxInherit, harness.DefaultName, harness.ClaudeSandboxOn, false},
		{harness.DefaultName, harness.ClaudeSandboxOff, harness.CopilotName, harness.CopilotSandboxInherit, false},
		{harness.DefaultName, harness.ClaudeSandboxOff, harness.CopilotName, harness.CopilotSandboxOff, false},
	}
}

// The whole existing admission matrix must be byte-for-byte unchanged once the
// implementation axis travels with a launch — for a legacy blank row, for an
// explicit harness-builtin, and for stacked (whose modes still mean exactly
// what the matrix thinks they mean).
func TestSandboxLineageMatrixUnchangedAcrossNonSingleWallImplementations(t *testing.T) {
	for _, implementation := range []sandboxpolicy.Implementation{
		"", // legacy row / never chose
		sandboxpolicy.ImplementationHarnessBuiltin,
		sandboxpolicy.ImplementationStacked,
	} {
		for _, tc := range sandboxLineageMatrix() {
			got := spawnSandboxLineageAllowed(
				spawnLineageSandbox{
					Harness: tc.parentHarness, Mode: tc.parentMode,
					Implementation: implementation,
				},
				spawnLineageSandbox{
					Harness: tc.childHarness, Mode: tc.childMode,
					Implementation: implementation,
				},
			)
			require.Equalf(t, tc.allowed, got,
				"impl=%q %s/%s -> %s/%s", implementation,
				tc.parentHarness, tc.parentMode, tc.childHarness, tc.childMode)
		}
	}
}

// A tclaude-layer child records its harness's no-confinement mode because
// tclaude's own wall is the one enforcing. The guard must classify it by the
// confinement it actually has, which is exactly the decision it got before the
// resolver started forcing that mode.
func TestSandboxLineageMapsTclaudeLayerChildToItsConfinementClass(t *testing.T) {
	layerClaude := spawnLineageSandbox{
		Harness: harness.DefaultName, Mode: harness.ClaudeSandboxOff,
		Implementation: sandboxpolicy.ImplementationTclaudeLayer,
	}
	layerCodex := spawnLineageSandbox{
		Harness: harness.CodexName, Mode: harness.SandboxDangerFull,
		Implementation: sandboxpolicy.ImplementationTclaudeLayer,
	}

	// Every parent that could mint the pre-forcing child (Claude `on` /
	// Codex managed-profile) can still mint the forced one.
	for _, parent := range []spawnLineageSandbox{
		{Harness: harness.DefaultName, Mode: harness.ClaudeSandboxInherit},
		{Harness: harness.DefaultName, Mode: harness.ClaudeSandboxOn},
		{Harness: harness.CodexName, Mode: harness.SandboxManagedProfile},
		{Harness: harness.OpenCodeName, Mode: harness.OpenCodeSandboxTclaudeLayer},
		{Harness: harness.OpenCodeName, Mode: harness.OpenCodeSandboxAccessControl},
	} {
		require.Truef(t, spawnSandboxLineageAllowed(parent, layerClaude),
			"parent %s/%s must still admit a tclaude-layer Claude child",
			parent.Harness, parent.Mode)
		require.Truef(t, spawnSandboxLineageAllowed(parent, layerCodex),
			"parent %s/%s must still admit a tclaude-layer Codex child",
			parent.Harness, parent.Mode)
	}

	// And a parent that could NOT mint the pre-forcing child still cannot: the
	// mapping restores the old class, it does not widen it.
	workspace := spawnLineageSandbox{Harness: harness.CodexName, Mode: harness.SandboxWorkspaceWrite}
	require.False(t, spawnSandboxLineageAllowed(workspace, layerClaude))
	require.False(t, spawnSandboxLineageAllowed(workspace, layerCodex))
	readOnly := spawnLineageSandbox{Harness: harness.CodexName, Mode: harness.SandboxReadOnly}
	require.False(t, spawnSandboxLineageAllowed(readOnly, layerCodex))
}

// The one place the mapping does NOT reproduce main's verdict, pinned here so
// it is a decision rather than an accident.
//
// A Codex `read-only` (or `workspace-write`) parent could previously mint a
// child that REQUESTED the same mode with tclaude-layer, because the guard
// judged the request. That child does not run read-only: the launch forces
// `danger-full-access` inside tclaude's wall, whose default host-open posture
// writes its cwd subtree. Judging the mode the child actually launches under
// closes that escalation, and the confinement class it maps to — the Codex
// managed profile — is correctly out of reach for these parents.
//
// This is a user-visible refusal on a spawn that used to succeed, and is called
// out in the PR description rather than claimed as decision-preserving.
func TestSandboxLineageRefusesTclaudeLayerChildFromNarrowerCodexParent(t *testing.T) {
	layerCodex := spawnLineageSandbox{
		Harness: harness.CodexName, Mode: harness.SandboxDangerFull,
		Implementation: sandboxpolicy.ImplementationTclaudeLayer,
	}
	for _, parentMode := range []string{harness.SandboxReadOnly, harness.SandboxWorkspaceWrite} {
		parent := spawnLineageSandbox{Harness: harness.CodexName, Mode: parentMode}
		require.Falsef(t, spawnSandboxLineageAllowed(parent, layerCodex),
			"codex %s parent must not mint a tclaude-walled child", parentMode)
		// The same parent minting the same mode WITHOUT the outer wall is
		// untouched: only the tclaude-layer shape moves.
		require.Truef(t, spawnSandboxLineageAllowed(parent,
			spawnLineageSandbox{Harness: harness.CodexName, Mode: parentMode}),
			"codex %s parent keeps minting its own class", parentMode)
	}
}

// The mapping keys on the EXACT tclaude-layer constant. `stacked` runs the
// harness's own sandbox nested inside tclaude's, so its Claude `off` really is
// a stood-down harness wall inside another one and must not borrow the
// single-wall class.
func TestSandboxLineageDoesNotMapStackedChildren(t *testing.T) {
	stackedOff := spawnLineageSandbox{
		Harness: harness.DefaultName, Mode: harness.ClaudeSandboxOff,
		Implementation: sandboxpolicy.ImplementationStacked,
	}
	inheritParent := spawnLineageSandbox{Harness: harness.DefaultName, Mode: harness.ClaudeSandboxInherit}
	require.False(t, spawnSandboxLineageAllowed(inheritParent, stackedOff),
		"a stacked Claude off child must keep the fully-open classification")

	stackedDanger := spawnLineageSandbox{
		Harness: harness.CodexName, Mode: harness.SandboxDangerFull,
		Implementation: sandboxpolicy.ImplementationStacked,
	}
	require.False(t, spawnSandboxLineageAllowed(inheritParent, stackedDanger))
}

// PR1 admits no Copilot lineage: a Copilot child stays refused whatever the
// implementation, including the single-wall one its detached spawning will
// eventually need (TCL-989 leaves that to a later step).
func TestSandboxLineageStillRefusesEveryCopilotChild(t *testing.T) {
	for _, implementation := range []sandboxpolicy.Implementation{
		"",
		sandboxpolicy.ImplementationHarnessBuiltin,
		sandboxpolicy.ImplementationTclaudeLayer,
		sandboxpolicy.ImplementationStacked,
		sandboxpolicy.ImplementationOff,
	} {
		for _, mode := range []string{
			harness.CopilotSandboxInherit, harness.CopilotSandboxOff, "",
		} {
			child := spawnLineageSandbox{
				Harness: harness.CopilotName, Mode: mode, Implementation: implementation,
			}
			require.Falsef(t, spawnSandboxLineageAllowed(
				spawnLineageSandbox{Harness: harness.DefaultName, Mode: harness.ClaudeSandboxOff},
				child,
			), "copilot child impl=%q mode=%q must stay refused", implementation, mode)
		}
	}
}

// A row written before the implementation column existed must read as
// harness-builtin — the posture it actually had — rather than as an unset value
// the mapping might treat specially.
func TestNormalizeLineageImplementationDefaultsToHarnessBuiltin(t *testing.T) {
	for _, raw := range []string{"", "   ", "not-an-implementation"} {
		require.Equalf(t, sandboxpolicy.ImplementationHarnessBuiltin,
			normalizeLineageImplementation(raw), "raw=%q", raw)
	}
	require.Equal(t, sandboxpolicy.ImplementationTclaudeLayer,
		normalizeLineageImplementation(string(sandboxpolicy.ImplementationTclaudeLayer)))
	require.Equal(t, sandboxpolicy.ImplementationStacked,
		normalizeLineageImplementation(string(sandboxpolicy.ImplementationStacked)))
}

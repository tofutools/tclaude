package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// Gemini enters the sandbox-lineage matrix in exactly the pair Copilot does —
// tclaude-layer with the harness's own sandbox off — and as the same class, so
// the matrix is pinned as an equivalence with the reviewed Copilot rows rather
// than as a second hand-written list that could drift from it.
func TestSandboxLineageGeminiMatchesTheCopilotPair(t *testing.T) {
	gemini := spawnLineageSandbox{
		Harness: harness.GeminiName, HarnessBuiltinMode: harness.GeminiSandboxOff,
		Implementation: sandboxpolicy.ImplementationTclaudeLayer,
	}
	copilot := spawnLineageSandbox{
		Harness: harness.CopilotName, HarnessBuiltinMode: harness.CopilotSandboxOff,
		Implementation: sandboxpolicy.ImplementationTclaudeLayer,
	}
	others := append(copilotLineageParents(),
		spawnLineageSandbox{Harness: harness.ShellName, HarnessBuiltinMode: harness.ShellSandboxOff,
			Implementation: sandboxpolicy.ImplementationTclaudeLayer},
		spawnLineageSandbox{Harness: harness.DefaultName, HarnessBuiltinMode: harness.ClaudeSandboxOn},
		spawnLineageSandbox{Harness: harness.CodexName, HarnessBuiltinMode: harness.SandboxReadOnly},
	)
	for _, other := range others {
		assert.Equal(t, spawnSandboxLineageAllowed(other, copilot), spawnSandboxLineageAllowed(other, gemini),
			"parent %s may mint Gemini exactly when it may mint Copilot", lineageKey(other))
		assert.Equal(t, spawnSandboxLineageAllowed(copilot, other), spawnSandboxLineageAllowed(gemini, other),
			"a walled Gemini parent delegates %s exactly as a walled Copilot parent does", lineageKey(other))
	}
	assert.True(t, spawnSandboxLineageAllowed(gemini, gemini))
	assert.True(t, spawnSandboxLineageAllowed(gemini, copilot))
	assert.True(t, spawnSandboxLineageAllowed(copilot, gemini))

	// Every other Gemini row is outside the matrix on both sides: `off`
	// without the outer wall is an unconfined agent, and `inherit` is decided
	// by settings tclaude does not control.
	for _, unproven := range []spawnLineageSandbox{
		{Harness: harness.GeminiName, HarnessBuiltinMode: harness.GeminiSandboxOff,
			Implementation: sandboxpolicy.ImplementationHarnessBuiltin},
		{Harness: harness.GeminiName, HarnessBuiltinMode: harness.GeminiSandboxOff,
			Implementation: sandboxpolicy.ImplementationOff},
		{Harness: harness.GeminiName, HarnessBuiltinMode: harness.GeminiSandboxInherit,
			Implementation: sandboxpolicy.ImplementationTclaudeLayer},
		{Harness: harness.GeminiName},
	} {
		assert.False(t, spawnSandboxLineageAllowed(
			spawnLineageSandbox{Harness: harness.DefaultName, HarnessBuiltinMode: harness.ClaudeSandboxOn}, unproven),
			"child %+v", unproven)
		assert.False(t, spawnSandboxLineageAllowed(unproven, gemini), "parent %+v", unproven)
	}

	remedy := copilotLineageRemedy(
		spawnLineageSandbox{Harness: harness.DefaultName, HarnessBuiltinMode: harness.ClaudeSandboxOn},
		spawnLineageSandbox{Harness: harness.GeminiName})
	assert.Contains(t, remedy, "tclaude-layer")
	assert.Contains(t, remedy, harness.GeminiName)
}

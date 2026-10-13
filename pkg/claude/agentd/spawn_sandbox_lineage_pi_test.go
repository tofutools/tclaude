package agentd

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// pi has no sandbox, so its only confined launch is tclaude's wall around pi's
// `off`. That launch joins the walled-off class with Copilot's and Gemini's;
// a pi outside the wall, or stacked, is no proof of anything.
func TestSandboxLineagePiOnlyTheWalledLaunchIsProven(t *testing.T) {
	walled := spawnLineageSandbox{
		Harness: harness.PiName, HarnessBuiltinMode: harness.PiSandboxOff,
		Implementation: sandboxpolicy.ImplementationTclaudeLayer,
	}
	walledGemini := spawnLineageSandbox{
		Harness: harness.GeminiName, HarnessBuiltinMode: harness.GeminiSandboxOff,
		Implementation: sandboxpolicy.ImplementationTclaudeLayer,
	}
	require.True(t, piProvenLineageLaunch(walled))
	require.True(t, spawnSandboxLineageAllowed(walled, walled), "a walled pi may spawn its own kind")
	require.True(t, spawnSandboxLineageAllowed(walled, walledGemini), "the walled-off class delegates as one")
	require.True(t, spawnSandboxLineageAllowed(walledGemini, walled))

	for _, unproven := range []spawnLineageSandbox{
		{Harness: harness.PiName, HarnessBuiltinMode: harness.PiSandboxOff,
			Implementation: sandboxpolicy.ImplementationHarnessBuiltin},
		{Harness: harness.PiName, HarnessBuiltinMode: harness.PiSandboxOff,
			Implementation: sandboxpolicy.ImplementationStacked},
		{Harness: harness.PiName, HarnessBuiltinMode: ""},
	} {
		require.False(t, piProvenLineageLaunch(unproven))
		require.Falsef(t, spawnSandboxLineageAllowed(walled, unproven),
			"a walled pi may not mint an unconfined one: %s", lineageKey(unproven))
		require.Falsef(t, spawnSandboxLineageAllowed(unproven, walled),
			"an unconfined pi proves nothing: %s", lineageKey(unproven))
	}
}

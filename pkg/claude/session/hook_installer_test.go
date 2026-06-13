package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// TestCCHookInstaller_AttachedToClaudeDescriptor pins that the CC hook
// installer is wired onto the claude harness so `tclaude setup` can
// dispatch hook install through the seam, and that it delegates to this
// package's canonical CC hook functions.
func TestCCHookInstaller_AttachedToClaudeDescriptor(t *testing.T) {
	h := harness.Default()
	require.True(t, h.SupportsHooks(), "claude harness must expose a HookInstaller")
	require.NotNil(t, h.Hooks)

	// The adapter delegates to the canonical CC functions.
	assert.Equal(t, ClaudeSettingsPath(), h.Hooks.ConfigTarget(), "ConfigTarget must be the claude settings path")
	assert.Equal(t, "", h.Hooks.TrustNote(), "Claude Code needs no hook-trust step")

	// Check() delegates to CheckHooksInstalled — both must agree.
	wantInstalled, wantMissing, wantRepair := CheckHooksInstalled()
	gotInstalled, gotMissing, gotRepair := h.Hooks.Check()
	assert.Equal(t, wantInstalled, gotInstalled)
	// missing is a set built by iterating a map, so its order is not
	// stable between calls — compare membership, not order (otherwise this
	// flakes whenever missing is non-empty, e.g. on a clean CI home).
	assert.ElementsMatch(t, wantMissing, gotMissing)
	assert.Equal(t, wantRepair, gotRepair)
}

package session

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// A --permission-profile the single-wall implementation discards must still be
// JUDGED. The discard moved ahead of the checks that used to see it (TCL-989),
// so without this the three combinations below would launch silently while
// every other implementation refuses them.
func TestValidateReplacedPermissionProfileKeepsTheRefusals(t *testing.T) {
	claude, err := harness.Resolve(harness.DefaultName)
	require.NoError(t, err)
	codex, err := harness.Resolve(harness.CodexName)
	require.NoError(t, err)

	// The realistic shape: a human passes no --sandbox at all, so the profile
	// flag is judged on the harness rule rather than on exclusivity.
	err = validateReplacedPermissionProfile(claude, "", "foo")
	require.Error(t, err, "a permission profile on a non-Codex harness must refuse")
	require.Contains(t, err.Error(), "Codex launch option")

	// With an explicit mode beside it, exclusivity is checked first — the same
	// order runNew applies to every other implementation.
	err = validateReplacedPermissionProfile(claude, harness.ClaudeSandboxOn, "foo")
	require.Error(t, err)
	require.Contains(t, err.Error(), "mutually exclusive")

	err = validateReplacedPermissionProfile(codex, harness.SandboxReadOnly, "foo")
	require.Error(t, err, "--permission-profile and an explicit --sandbox stay exclusive")
	require.Contains(t, err.Error(), "mutually exclusive")

	err = validateReplacedPermissionProfile(codex, harness.SandboxManagedProfile, "other")
	require.Error(t, err, "a profile conflicting with the managed pseudo-mode must refuse")
	require.Contains(t, err.Error(), harness.CodexAgentProfile)
}

// The combinations a non-tclaude-layer launch accepts must keep launching:
// this gate restores the old refusals, it does not invent new ones.
func TestValidateReplacedPermissionProfileAcceptsTheValidPairs(t *testing.T) {
	claude, err := harness.Resolve(harness.DefaultName)
	require.NoError(t, err)
	codex, err := harness.Resolve(harness.CodexName)
	require.NoError(t, err)
	copilot, err := harness.Resolve(harness.CopilotName)
	require.NoError(t, err)

	// No profile at all — the overwhelmingly common shape.
	for _, h := range []*harness.Harness{claude, codex, copilot} {
		require.NoErrorf(t, validateReplacedPermissionProfile(h, "", ""), "harness %s", h.Name)
	}
	require.NoError(t, validateReplacedPermissionProfile(claude, harness.ClaudeSandboxOn, ""))

	// The managed pseudo-mode CONSUMES the mode selecting the profile, so the
	// pair is not a mutual-exclusion violation — matching runNew's own
	// normalization, which resolves it rather than refusing.
	require.NoError(t, validateReplacedPermissionProfile(
		codex, harness.SandboxManagedProfile, ""))
	require.NoError(t, validateReplacedPermissionProfile(
		codex, harness.SandboxManagedProfile, harness.CodexAgentProfile))

	// A Codex profile with no explicit --sandbox is the supported pairing.
	require.NoError(t, validateReplacedPermissionProfile(codex, "", "custom-profile"))
}

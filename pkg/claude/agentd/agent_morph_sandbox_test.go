package agentd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// The morph preview claims a harness's own sandbox confines the agent only
// when the validated mode does; otherwise it says it cannot confirm, or warns.
func TestMorphSandboxViewHarnessBuiltinClaimsOnlyWhatTheModeConfines(t *testing.T) {
	get := func(name string) *harness.Harness {
		h, ok := harness.Get(name)
		require.True(t, ok)
		return h
	}
	builtin := sandboxpolicy.ImplementationHarnessBuiltin
	codex, claude, opencode := get(harness.CodexName), get(harness.DefaultName), get(harness.OpenCodeName)

	v := viewMorphSandbox("", builtin, codex, harness.SandboxWorkspaceWrite)
	assert.Equal(t, "not used: the harness's own sandbox confines it", v.Tclaude)
	assert.False(t, v.Unsandboxed)

	v = viewMorphSandbox("", builtin, codex, harness.SandboxDangerFull)
	assert.True(t, v.Unsandboxed, "Codex's off mode is no sandbox")
	assert.NotEmpty(t, v.Warning)

	v = viewMorphSandbox("", builtin, claude, harness.ClaudeSandboxInherit)
	assert.False(t, v.Unsandboxed)
	assert.NotContains(t, v.Tclaude, "confines", "inherit depends on settings.json: no claim")
	assert.Contains(t, v.Harness, "tclaude cannot confirm a sandbox")

	v = viewMorphSandbox("", builtin, opencode, opencode.Sandbox.DefaultMode())
	assert.True(t, v.Unsandboxed, "OpenCode's access control is not an OS sandbox")

	v = viewMorphSandbox("", sandboxpolicy.ImplementationOff, codex, harness.SandboxDangerFull)
	assert.True(t, v.Unsandboxed)
	assert.Equal(t, "codex danger-full-access (no sandbox)", v.Harness)
}

package agentd

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestSandboxProfileCapabilityFailureRequiresClaudeOnWithDeny(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	snapshot := &sandboxpolicy.Snapshot{Effective: sandboxpolicy.EffectiveProfile{
		Filesystem: []sandboxpolicy.FilesystemGrant{{Path: root, Access: sandboxpolicy.AccessDeny}},
	}}

	require.Nil(t, sandboxProfileCapabilityFailure(harness.DefaultName, harness.ClaudeSandboxOn, snapshot))
	for _, mode := range []string{harness.ClaudeSandboxOff, harness.ClaudeSandboxInherit, ""} {
		failure := sandboxProfileCapabilityFailure(harness.DefaultName, mode, snapshot)
		require.NotNil(t, failure)
		require.Contains(t, failure.Msg, `require sandbox "on"`)
	}
}

func TestSandboxProfileCapabilityFailureIgnoresMissingAllowRulesButRejectsMissingDeny(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	missing := filepath.Join(root, "future")
	snapshot := &sandboxpolicy.Snapshot{Effective: sandboxpolicy.EffectiveProfile{
		Filesystem: []sandboxpolicy.FilesystemGrant{{
			Path: missing, Access: sandboxpolicy.AccessRead,
		}},
	}}

	require.Nil(t, sandboxProfileCapabilityFailure(harness.DefaultName, harness.ClaudeSandboxOff, snapshot))
	require.Nil(t, sandboxProfileCapabilityFailure(harness.CodexName, harness.SandboxReadOnly, snapshot))
	snapshot.Effective.Filesystem[0].Access = sandboxpolicy.AccessDeny
	failure := sandboxProfileCapabilityFailure(harness.DefaultName, harness.ClaudeSandboxOn, snapshot)
	require.NotNil(t, failure)
	require.Contains(t, failure.Msg, "cannot be enforced")
}

func TestSandboxProfilesDisabledOnlyForCodexDangerFullAccess(t *testing.T) {
	require.True(t, sandboxProfilesDisabled(harness.CodexName, harness.SandboxDangerFull))
	require.False(t, sandboxProfilesDisabled(harness.CodexName, harness.SandboxManagedProfile))
	require.False(t, sandboxProfilesDisabled(harness.CodexName, harness.SandboxReadOnly))
	require.False(t, sandboxProfilesDisabled(harness.DefaultName, harness.ClaudeSandboxOff))
}

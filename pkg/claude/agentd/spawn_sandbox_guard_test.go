package agentd

import (
	"path/filepath"
	"runtime"
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

func TestSandboxProfileCapabilityFailureGatesSemanticReadExclusions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	snapshot := &sandboxpolicy.Snapshot{Effective: sandboxpolicy.EffectiveProfile{
		ReadBaselineExclusions: []string{sandboxpolicy.ReadExclusionSSH},
	}}
	require.Nil(t, sandboxProfileCapabilityFailure(harness.DefaultName, harness.ClaudeSandboxOn, snapshot))
	require.Nil(t, sandboxProfileCapabilityFailure(harness.CodexName, harness.SandboxManagedProfile, snapshot))
	for _, tc := range []struct{ harness, mode string }{
		{harness.DefaultName, harness.ClaudeSandboxInherit},
		{harness.CodexName, harness.SandboxReadOnly},
	} {
		failure := sandboxProfileCapabilityFailure(tc.harness, tc.mode, snapshot)
		require.NotNil(t, failure)
		require.Equal(t, harness.SandboxCapabilityReadExclusions, failure.Kind)
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

func TestSandboxProfileCapabilityFailureRejectsUnsupportedNetworkOnlyProfile(t *testing.T) {
	snapshot := &sandboxpolicy.Snapshot{Effective: sandboxpolicy.EffectiveProfile{
		NetworkAccess: sandboxpolicy.NetworkAccessInternet,
	}}

	require.Nil(t, sandboxProfileCapabilityFailure(harness.CodexName, harness.SandboxManagedProfile, snapshot))
	for _, tc := range []struct {
		harness string
		mode    string
	}{
		{harness.DefaultName, harness.ClaudeSandboxOn},
		{harness.CodexName, harness.SandboxReadOnly},
		{harness.CodexName, harness.SandboxDangerFull},
	} {
		failure := sandboxProfileCapabilityFailure(tc.harness, tc.mode, snapshot)
		require.NotNil(t, failure)
		require.Equal(t, "unsupported_sandbox_profile_network", failure.Kind)
	}

	snapshot.Effective.NetworkAccess = sandboxpolicy.NetworkAccessNone
	failure := sandboxProfileCapabilityFailure(harness.CodexName, harness.SandboxManagedProfile, snapshot)
	if runtime.GOOS == "linux" {
		require.NotNil(t, failure)
		require.Equal(t, "unsupported_sandbox_profile_network", failure.Kind)
		require.Contains(t, failure.Msg, "agentd Unix socket")
	} else {
		require.Nil(t, failure)
	}
}

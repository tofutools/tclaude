package agentd

import (
	"os"
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

func TestSandboxProfileCapabilityFailureGatesReopenUnderDeny(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	workspace := filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(workspace, 0o755))
	snapshot := &sandboxpolicy.Snapshot{Effective: sandboxpolicy.EffectiveProfile{
		Filesystem: []sandboxpolicy.FilesystemGrant{
			{Path: root, Access: sandboxpolicy.AccessDeny},
			{Path: workspace, Access: sandboxpolicy.AccessRead},
		},
	}}
	require.Nil(t, sandboxProfileCapabilityFailure(harness.DefaultName, harness.ClaudeSandboxOn, snapshot))
	for _, tc := range []struct{ harness, mode string }{
		{harness.DefaultName, harness.ClaudeSandboxInherit},
		{harness.CodexName, harness.SandboxReadOnly},
	} {
		failure := sandboxProfileCapabilityFailure(tc.harness, tc.mode, snapshot)
		require.NotNil(t, failure)
		require.Equal(t, harness.SandboxCapabilityReopenUnderDeny, failure.Kind)
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

func TestSandboxProfilesDisabledForExplicitNoContainmentModes(t *testing.T) {
	require.True(t, sandboxProfilesDisabled(harness.CodexName, harness.SandboxDangerFull))
	require.True(t, sandboxProfilesDisabled(harness.OpenCodeName, harness.OpenCodeSandboxOff))
	require.False(t, sandboxProfilesDisabled(harness.CodexName, harness.SandboxManagedProfile))
	require.False(t, sandboxProfilesDisabled(harness.CodexName, harness.SandboxReadOnly))
	require.False(t, sandboxProfilesDisabled(harness.DefaultName, harness.ClaudeSandboxOff))
	require.False(t, sandboxProfilesDisabled(harness.OpenCodeName, harness.OpenCodeSandboxAccessControl))
	require.False(t, sandboxProfilesDisabled(harness.OpenCodeName, ""))
}

func TestOpenCodePolicyRepresentabilityUsesAccessControlAndFailsClosedOtherwise(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	snapshot := &sandboxpolicy.Snapshot{Effective: sandboxpolicy.EffectiveProfile{
		Filesystem: []sandboxpolicy.FilesystemGrant{{Path: root, Access: sandboxpolicy.AccessRead}},
	}}
	require.Nil(t, sandboxProfileCapabilityFailure(
		harness.OpenCodeName, harness.OpenCodeSandboxAccessControl, snapshot))
	for _, mode := range []string{harness.OpenCodeSandboxOff, "", "unknown"} {
		failure := sandboxProfileCapabilityFailure(harness.OpenCodeName, mode, snapshot)
		require.NotNil(t, failure)
		require.Equal(t, "unsupported_sandbox_profile_filesystem", failure.Kind)
		require.Contains(t, failure.Msg, harness.OpenCodeSandboxAccessControl)
	}
}

func TestOpenCodeSandboxLineageClassifiesAccessControlOffAndUnknown(t *testing.T) {
	openCodeOff := spawnLineageSandbox{Harness: harness.OpenCodeName, Mode: harness.OpenCodeSandboxOff}
	require.True(t, spawnSandboxLineageAllowed(openCodeOff, openCodeOff))
	require.False(t, spawnSandboxLineageAllowed(
		spawnLineageSandbox{Harness: harness.DefaultName, Mode: harness.ClaudeSandboxOn},
		openCodeOff,
	))

	access := spawnLineageSandbox{Harness: harness.OpenCodeName, Mode: harness.OpenCodeSandboxAccessControl}
	require.True(t, spawnSandboxLineageAllowed(access, access))
	require.True(t, spawnSandboxLineageAllowed(access,
		spawnLineageSandbox{Harness: harness.DefaultName, Mode: harness.ClaudeSandboxOn}))
	require.True(t, spawnSandboxLineageAllowed(access,
		spawnLineageSandbox{Harness: harness.CodexName, Mode: harness.SandboxManagedProfile}))
	require.False(t, spawnSandboxLineageAllowed(access, openCodeOff))
	require.False(t, spawnSandboxLineageAllowed(
		spawnLineageSandbox{Harness: harness.OpenCodeName, Mode: ""},
		access,
	))
	require.False(t, spawnSandboxLineageAllowed(
		access,
		spawnLineageSandbox{Harness: harness.OpenCodeName, Mode: "unknown"},
	))
}

func TestSandboxProfileCapabilityFailureRejectsUnsupportedNetworkOnlyProfile(t *testing.T) {
	snapshot := &sandboxpolicy.Snapshot{Effective: sandboxpolicy.EffectiveProfile{
		NetworkAccess: sandboxpolicy.NetworkAccessInternet,
	}}

	require.Nil(t, sandboxProfileCapabilityFailure(harness.CodexName, harness.SandboxManagedProfile, snapshot))
	require.Nil(t, sandboxProfileCapabilityFailure(harness.OpenCodeName, harness.OpenCodeSandboxAccessControl, snapshot))
	for _, tc := range []struct {
		harness string
		mode    string
	}{
		{harness.DefaultName, harness.ClaudeSandboxOn},
		{harness.CodexName, harness.SandboxReadOnly},
		{harness.CodexName, harness.SandboxDangerFull},
		{harness.OpenCodeName, harness.OpenCodeSandboxOff},
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

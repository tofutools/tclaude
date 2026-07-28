package agentd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/session"
)

func TestPlanSandboxProfileAccessDisclosesUnmaterializedSocketEntries(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	regular := filepath.Join(root, "regular")
	require.NoError(t, os.WriteFile(regular, []byte("not a socket"), 0o600))
	missing := filepath.Join(root, "missing.sock")
	unmatched := filepath.Join(root, "future-*.sock")
	snapshot := &sandboxpolicy.Snapshot{Effective: sandboxpolicy.EffectiveProfile{
		UnixSockets: &sandboxpolicy.UnixSocketRules{
			Mode: sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.SocketAllowEntry{
				{Path: missing},
				{Path: regular},
				{PathGlob: unmatched},
			},
		},
	}}

	notices, failure := planSandboxProfileAccessForLaunch(
		harness.CodexName,
		harness.SandboxManagedProfile,
		snapshot,
		string(sandboxpolicy.ImplementationHarnessBuiltin),
		session.ModelTransportLaunchContext{},
	)
	require.Nil(t, failure)
	var notice *sandboxpolicy.AccessNotice
	for i := range notices {
		if notices[i].Class == sandboxpolicy.AccessNoticeClassLaunch {
			notice = &notices[i]
			break
		}
	}
	require.NotNil(t, notice)
	require.NotNil(t, snapshot.UnixSocketMaterialization)
	require.Empty(t, snapshot.UnixSocketMaterialization.Paths)
	require.Equal(t, []string{missing, regular, unmatched},
		snapshot.UnixSocketMaterialization.Unmaterialized)
	require.Contains(t, snapshot.Effective.AccessNotices, *notice)
	require.Equal(t, sandboxpolicy.AccessNoticeClassLaunch, notice.Class)
	require.Equal(t, sandboxpolicy.AccessNoticeReasonUnmaterializedEntries, notice.Reason)
	require.Equal(t, []int{0, 1, 2}, notice.Entries)
	for _, selector := range []string{missing, regular, unmatched} {
		require.True(t, strings.Contains(notice.Detail, selector),
			"launch notice must name unmaterialized selector %q: %s",
			selector, notice.Detail)
	}
}

func TestPlanSandboxProfileAccessPersistsDetectedProbeWhenVerdictCannotFlip(t *testing.T) {
	oldProbe := probeFilteredNetworkPrerequisite
	oldVerdict := resolveTclaudeLayerAccessVerdict
	t.Cleanup(func() {
		probeFilteredNetworkPrerequisite = oldProbe
		resolveTclaudeLayerAccessVerdict = oldVerdict
	})
	resolveTclaudeLayerAccessVerdict = func(
		string, sandboxpolicy.NetworkPosture,
	) (harness.LaunchOSSandbox, error) {
		return harness.LaunchOSSandbox{State: "on", Source: "test bwrap"}, nil
	}
	probeFilteredNetworkPrerequisite = func() session.FilteredNetworkPrerequisite {
		return session.FilteredNetworkPrerequisite{
			Detected: true,
			Detail:   "namespace execution passed; pasta and nft executables found; gateway not verified",
		}
	}
	snapshot := &sandboxpolicy.Snapshot{Effective: sandboxpolicy.EffectiveProfile{
		Network: &sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.NetworkAllowEntry{{
				CIDR: "192.0.2.0/24", Ports: []int{443},
			}},
		},
	}}

	notices, failure := planSandboxProfileAccessForLaunch(
		harness.DefaultName,
		harness.ClaudeSandboxOff,
		snapshot,
		string(sandboxpolicy.ImplementationTclaudeLayer),
		session.ModelTransportLaunchContext{},
	)
	require.Nil(t, failure)
	require.Len(t, notices, 2)
	require.Equal(t, "no_mechanism", notices[0].Reason,
		"a non-filtered launch verdict must retain the widening authority")
	require.Equal(t, sandboxpolicy.AccessNoticeReasonFilteredPrerequisite, notices[1].Reason)
	require.Contains(t, notices[1].Detail, "prerequisite probe: detected")
	require.Contains(t, notices[1].Detail, "launch cannot consume")
	require.Contains(t, notices[1].Detail, "outbound remains open")
	require.Contains(t, snapshot.Effective.AccessNotices, notices[0])
	require.Contains(t, snapshot.Effective.AccessNotices, notices[1])

	planned, err := sandboxpolicy.PlannedEffectiveAccessAxes(snapshot.Effective)
	require.NoError(t, err)
	require.Equal(t, sandboxpolicy.AccessModeOpen, planned.Network.Mode,
		"a ready probe without a filtered launch verdict must not activate enforcement")
}

func TestPlanSandboxProfileAccessActivatesReadyOpenCodeWithExplicitProvider(t *testing.T) {
	oldProbe := probeFilteredNetworkPrerequisite
	oldVerdict := resolveTclaudeLayerAccessVerdict
	t.Cleanup(func() {
		probeFilteredNetworkPrerequisite = oldProbe
		resolveTclaudeLayerAccessVerdict = oldVerdict
	})
	resolveTclaudeLayerAccessVerdict = func(
		_ string, posture sandboxpolicy.NetworkPosture,
	) (harness.LaunchOSSandbox, error) {
		return harness.LaunchOSSandbox{
			State: "on", Source: "test bwrap",
			FilteredNetwork: posture == sandboxpolicy.NetworkFiltered,
		}, nil
	}
	newSnapshot := func() *sandboxpolicy.Snapshot {
		return &sandboxpolicy.Snapshot{Effective: sandboxpolicy.EffectiveProfile{
			Environment: []sandboxpolicy.EnvironmentEntry{{
				Name:  "OPENCODE_CONFIG_CONTENT",
				Value: `{"enabled_providers":["corp"],"provider":{"corp":{"npm":"@ai-sdk/openai-compatible","whitelist":["model"],"models":{"model":{}},"options":{"baseURL":"https://models.example","apiKey":"test-key"}}}}`,
			}},
			Network: &sandboxpolicy.NetworkRules{
				Mode: sandboxpolicy.AccessModeList,
				Allow: []sandboxpolicy.NetworkAllowEntry{{
					Domain: "models.example", Ports: []int{443},
				}},
			},
		}}
	}

	probeFilteredNetworkPrerequisite = func() session.FilteredNetworkPrerequisite {
		return session.FilteredNetworkPrerequisite{
			Detected: false,
			Detail:   "pasta unavailable",
		}
	}
	notices, failure := planSandboxProfileAccessForLaunch(
		harness.OpenCodeName,
		harness.OpenCodeSandboxTclaudeLayer,
		newSnapshot(),
		string(sandboxpolicy.ImplementationTclaudeLayer),
		session.ModelTransportLaunchContext{},
	)
	require.Nil(t, failure)
	require.Len(t, notices, 2)
	require.Equal(t, "no_mechanism", notices[0].Reason)
	require.Contains(t, notices[1].Detail, "prerequisite probe: unavailable")

	probeFilteredNetworkPrerequisite = func() session.FilteredNetworkPrerequisite {
		return session.FilteredNetworkPrerequisite{
			Detected: true,
			Detail:   "namespace, pasta, and nft detected",
		}
	}
	_, failure = planSandboxProfileAccessForLaunch(
		harness.OpenCodeName,
		harness.OpenCodeSandboxTclaudeLayer,
		newSnapshot(),
		string(sandboxpolicy.ImplementationTclaudeLayer),
		session.ModelTransportLaunchContext{Model: "corp/model"},
	)
	require.Nil(t, failure)
}

func TestPlanSandboxProfileAccessMintsModelTransportFromLaunchContext(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("filtered gateway capability is Linux-only")
	}
	oldProbe := probeFilteredNetworkPrerequisite
	oldVerdict := resolveTclaudeLayerAccessVerdict
	t.Cleanup(func() {
		probeFilteredNetworkPrerequisite = oldProbe
		resolveTclaudeLayerAccessVerdict = oldVerdict
	})
	probeFilteredNetworkPrerequisite = func() session.FilteredNetworkPrerequisite {
		return session.FilteredNetworkPrerequisite{
			Detected: true,
			Detail:   "namespace, pasta, and nft detected",
		}
	}
	resolveTclaudeLayerAccessVerdict = func(
		string, sandboxpolicy.NetworkPosture,
	) (harness.LaunchOSSandbox, error) {
		return harness.LaunchOSSandbox{
			State: "on", Source: "test filtered bwrap", FilteredNetwork: true,
		}, nil
	}
	codexHome := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(codexHome, "auth.json"), []byte(
		`{"auth_mode":"apikey","OPENAI_API_KEY":"test-key"}`), 0o600))
	snapshot := &sandboxpolicy.Snapshot{Effective: sandboxpolicy.EffectiveProfile{
		Environment: []sandboxpolicy.EnvironmentEntry{{
			Name: "CODEX_HOME", Value: codexHome,
		}},
		Network: &sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.NetworkAllowEntry{
				{Domain: "api.openai.com", Ports: []int{443}},
			},
		},
	}}

	notices, failure := planSandboxProfileAccessForLaunch(
		harness.CodexName,
		harness.SandboxDangerFull,
		snapshot,
		string(sandboxpolicy.ImplementationTclaudeLayer),
		session.ModelTransportLaunchContext{
			Model: "gpt-5.4", Cwd: t.TempDir(),
		},
	)
	require.Nil(t, failure)
	var found bool
	for _, notice := range notices {
		if notice.Reason == sandboxpolicy.AccessNoticeReasonFilteredModelTraffic {
			found = true
			require.Contains(t, notice.Detail, "api.openai.com:443")
			require.Contains(t, notice.Detail, "empirically audited")
		}
	}
	require.True(t, found, "resolved launch must persist the model-transport why-clause")
}

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

func TestSandboxProfileCapabilityFailureDefersTclaudeLayerPolicyToOuterApplier(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	snapshot := &sandboxpolicy.Snapshot{Effective: sandboxpolicy.EffectiveProfile{
		Filesystem: []sandboxpolicy.FilesystemGrant{
			{Path: root, Access: sandboxpolicy.AccessDeny},
		},
		NetworkAccess: sandboxpolicy.NetworkAccessNone,
	}}
	require.Nil(t, sandboxProfileCapabilityFailure(
		harness.DefaultName,
		harness.ClaudeSandboxOff,
		snapshot,
		string(sandboxpolicy.ImplementationTclaudeLayer),
	))
	require.Nil(t, sandboxProfileCapabilityFailure(
		harness.CodexName,
		harness.SandboxDangerFull,
		snapshot,
		string(sandboxpolicy.ImplementationTclaudeLayer),
	))
	require.Nil(t, sandboxProfileCapabilityFailure(
		harness.OpenCodeName,
		harness.OpenCodeSandboxTclaudeLayer,
		snapshot,
		string(sandboxpolicy.ImplementationTclaudeLayer),
	))
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
	require.False(t, sandboxProfilesDisabled(harness.OpenCodeName, harness.OpenCodeSandboxOff))
	require.False(t, sandboxProfilesDisabled(harness.CodexName, harness.SandboxManagedProfile))
	require.False(t, sandboxProfilesDisabled(harness.CodexName, harness.SandboxReadOnly))
	require.False(t, sandboxProfilesDisabled(harness.DefaultName, harness.ClaudeSandboxOff))
	require.False(t, sandboxProfilesDisabled(harness.OpenCodeName, harness.OpenCodeSandboxAccessControl))
	require.False(t, sandboxProfilesDisabled(harness.OpenCodeName, harness.OpenCodeSandboxTclaudeLayer))
	require.False(t, sandboxProfilesDisabled(harness.OpenCodeName, ""))
}

func TestOpenCodeSandboxModeAndImplementationMustAgree(t *testing.T) {
	require.Nil(t, sandboxProfileCapabilityFailure(
		harness.OpenCodeName,
		harness.OpenCodeSandboxTclaudeLayer,
		nil,
		string(sandboxpolicy.ImplementationTclaudeLayer),
	))
	for _, tc := range []struct {
		mode           string
		implementation string
	}{
		{harness.OpenCodeSandboxTclaudeLayer, string(sandboxpolicy.ImplementationHarnessBuiltin)},
		{harness.OpenCodeSandboxOff, string(sandboxpolicy.ImplementationTclaudeLayer)},
	} {
		failure := sandboxProfileCapabilityFailure(
			harness.OpenCodeName, tc.mode, nil, tc.implementation)
		require.NotNil(t, failure)
		require.Equal(t, "invalid_sandbox", failure.Kind)
	}
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

func TestOpenCodeSandboxLineageClassifiesLayerAccessControlOffAndUnknown(t *testing.T) {
	openCodeOff := spawnLineageSandbox{Harness: harness.OpenCodeName, Mode: harness.OpenCodeSandboxOff}
	require.True(t, spawnSandboxLineageAllowed(openCodeOff, openCodeOff))
	require.False(t, spawnSandboxLineageAllowed(
		spawnLineageSandbox{Harness: harness.DefaultName, Mode: harness.ClaudeSandboxOn},
		openCodeOff,
	))

	access := spawnLineageSandbox{Harness: harness.OpenCodeName, Mode: harness.OpenCodeSandboxAccessControl}
	layer := spawnLineageSandbox{Harness: harness.OpenCodeName, Mode: harness.OpenCodeSandboxTclaudeLayer}
	require.True(t, spawnSandboxLineageAllowed(access, access))
	require.True(t, spawnSandboxLineageAllowed(access, layer))
	require.True(t, spawnSandboxLineageAllowed(layer, layer))
	require.False(t, spawnSandboxLineageAllowed(layer, access))
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

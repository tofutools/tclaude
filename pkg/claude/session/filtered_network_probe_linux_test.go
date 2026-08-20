//go:build linux

package session

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestTclaudeLayerNetworkPostureUsesPlannedDenyRows(t *testing.T) {
	effective := sandboxpolicy.EffectiveProfile{
		Network: &sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeOpen,
			Deny: []sandboxpolicy.NetworkAllowEntry{{CIDR: "192.0.2.0/24"}},
		},
	}
	posture, err := TclaudeLayerNetworkPosture(effective)
	require.NoError(t, err)
	assert.Equal(t, sandboxpolicy.NetworkFiltered, posture)

	effective.AccessNotices = []sandboxpolicy.AccessNotice{{
		Class:   sandboxpolicy.AccessNoticeClassDegradation,
		Axis:    "network",
		Reason:  "deny_selector_unsupported",
		Effect:  sandboxpolicy.AccessNoticeEffectNotEnforced,
		Entries: []int{0},
	}}
	posture, err = TclaudeLayerNetworkPosture(effective)
	require.NoError(t, err)
	assert.Equal(t, sandboxpolicy.NetworkHostOpen, posture,
		"an omitted unsupported deny must not demand a filtered runtime")
}

func TestFilteredNetworkPrerequisiteProbeNamesEveryBuildingBlock(t *testing.T) {
	oldBwrapPath := lookPathBwrap
	oldBwrapProbe := probeBwrap
	oldFilteredPath := filteredNetworkLookPath
	oldFilteredInspect := inspectFilteredNetworkPasta
	t.Cleanup(func() {
		lookPathBwrap = oldBwrapPath
		probeBwrap = oldBwrapProbe
		filteredNetworkLookPath = oldFilteredPath
		inspectFilteredNetworkPasta = oldFilteredInspect
	})
	stubTrustedExecutableWalk(t)
	lookPathBwrap = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	probeBwrap = func(_ string, posture sandboxpolicy.NetworkPosture, _ sandboxpolicy.RootPosture) error {
		assert.Equal(t, sandboxpolicy.NetworkFiltered, posture)
		return nil
	}
	filteredNetworkLookPath = func(name string) (string, error) {
		return "/usr/bin/" + name, nil
	}
	inspectFilteredNetworkPasta = func(string) (bool, error) { return true, nil }

	got := ProbeFilteredNetworkPrerequisite()
	require.True(t, got.Detected)
	assert.Contains(t, got.Detail, "bubblewrap")
	assert.Contains(t, got.Detail, "user/network namespace")
	assert.Contains(t, got.Detail, "nsenter")
	assert.NotContains(t, got.Detail, "CAP_NET_BIND_SERVICE")
	assert.Contains(t, got.Detail, "pasta")
	assert.Contains(t, got.Detail, "nft")
	assert.Contains(t, got.Detail, "gated launch boundary")
	assert.Contains(t, got.LaunchWhy(true), "atomic nft policy")
	assert.NotContains(t, got.LaunchWhy(true), "outbound remains open")
}

func TestFilteredNetworkPastaCapabilityProbeRequiresExactGatewayControls(t *testing.T) {
	full := append(
		append([]string(nil), baseFilteredNetworkPastaOptions...),
		syntheticLoopbackFilteredNetworkPastaOptions...)
	help := strings.Join(full, "\n")
	synthetic, err := validatePastaCapabilities(help)
	require.NoError(t, err)
	assert.True(t, synthetic)

	// Dropping a BASE control is fatal: no tier can run without it.
	base := strings.ReplaceAll(help, "--pid", "--pidfile")
	base = strings.ReplaceAll(base, "--config-net", "--old--config-net")
	synthetic, err = validatePastaCapabilities(base)
	require.ErrorContains(t, err, "--config-net")
	assert.ErrorContains(t, err, "--pid")
	assert.False(t, synthetic)
	assert.NotContains(t, err.Error(), "--map-guest-addr")
}

// An Ubuntu 24.04 vintage pasta lacks exactly the three synthetic-mapping
// controls. That caps it at the private-namespace tier rather than refusing it.
func TestFilteredNetworkPastaCapabilityProbeAcceptsBaseTierPasta(t *testing.T) {
	full := append(
		append([]string(nil), baseFilteredNetworkPastaOptions...),
		syntheticLoopbackFilteredNetworkPastaOptions...)
	help := strings.Join(full, "\n")
	for _, option := range []string{
		"--map-guest-addr", "--map-host-loopback", "--no-splice",
	} {
		help = strings.ReplaceAll(help, option, "--old"+option)
	}
	synthetic, err := validatePastaCapabilities(help)
	require.NoError(t, err)
	assert.False(t, synthetic)
}

func TestFilteredNetworkPastaCapabilityProbeBoundsExecutionAndOutput(t *testing.T) {
	oldCommand := filteredNetworkPastaCommand
	oldTimeout := filteredNetworkPastaProbeTimeout
	oldLimit := filteredNetworkPastaHelpLimit
	t.Cleanup(func() {
		filteredNetworkPastaCommand = oldCommand
		filteredNetworkPastaProbeTimeout = oldTimeout
		filteredNetworkPastaHelpLimit = oldLimit
	})

	filteredNetworkPastaProbeTimeout = 10 * time.Millisecond
	filteredNetworkPastaCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sleep", "1")
	}
	_, err := inspectPastaCapabilities("/trusted/pasta")
	require.ErrorContains(t, err, "deadline exceeded")

	filteredNetworkPastaProbeTimeout = 5 * time.Second
	filteredNetworkPastaHelpLimit = 8
	filteredNetworkPastaCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/echo", "123456789")
	}
	_, err = inspectPastaCapabilities("/trusted/pasta")
	require.ErrorContains(t, err, "output exceeds")
}

func TestFilteredNetworkPrerequisiteProbeRefusesOlderPasta(t *testing.T) {
	oldBwrapPath := lookPathBwrap
	oldBwrapProbe := probeBwrap
	oldFilteredPath := filteredNetworkLookPath
	oldFilteredInspect := inspectFilteredNetworkPasta
	t.Cleanup(func() {
		lookPathBwrap = oldBwrapPath
		probeBwrap = oldBwrapProbe
		filteredNetworkLookPath = oldFilteredPath
		inspectFilteredNetworkPasta = oldFilteredInspect
	})
	stubTrustedExecutableWalk(t)
	lookPathBwrap = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	probeBwrap = func(string, sandboxpolicy.NetworkPosture, sandboxpolicy.RootPosture) error { return nil }
	filteredNetworkLookPath = func(name string) (string, error) {
		return "/usr/bin/" + name, nil
	}
	inspectFilteredNetworkPasta = func(string) (bool, error) {
		return false, errors.New("missing options: --config-net")
	}

	got := ProbeFilteredNetworkPrerequisite()
	require.False(t, got.Detected)
	assert.False(t, got.PrivateNamespaceDetected)
	assert.Contains(t, got.Detail, "pasta")
	assert.Contains(t, got.Detail, "--config-net")
	assert.Contains(t, got.LaunchWhy(false), "outbound remains open")
}

func TestFilteredNetworkProbeArgsBuildTheNamespaceShapeWithoutInSandboxCapability(t *testing.T) {
	args, err := tclaudeLayerProbeArgs(
		sandboxpolicy.NetworkFiltered, sandboxpolicy.RootConstructed)
	require.NoError(t, err)
	joined := strings.Join(args, " ")
	assert.Contains(t, joined,
		"--unshare-user --uid 0 --gid 0 --unshare-net --unshare-pid")
	// The base policy is installed by the supervisor via nsenter, so the probe
	// no longer grants or checks any in-sandbox capability.
	assert.NotContains(t, joined, "--cap-add")
	assert.NotContains(t, joined, "CAP_NET_ADMIN")
	assert.NotContains(t, joined, `case "$cap_eff" in`)
}

func TestFilteredNetworkPrerequisiteProbeReportsFirstMissingCapability(t *testing.T) {
	oldBwrapPath := lookPathBwrap
	oldBwrapProbe := probeBwrap
	oldFilteredPath := filteredNetworkLookPath
	t.Cleanup(func() {
		lookPathBwrap = oldBwrapPath
		probeBwrap = oldBwrapProbe
		filteredNetworkLookPath = oldFilteredPath
	})
	stubTrustedExecutableWalk(t)
	lookPathBwrap = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	probeBwrap = func(string, sandboxpolicy.NetworkPosture, sandboxpolicy.RootPosture) error { return nil }
	filteredNetworkLookPath = func(name string) (string, error) {
		if name == "pasta" {
			return "", errors.New("not found")
		}
		return "/usr/bin/" + name, nil
	}

	got := ProbeFilteredNetworkPrerequisite()
	require.False(t, got.Detected)
	assert.Contains(t, got.Detail, "pasta")
	assert.Contains(t, got.LaunchWhy(false), "unavailable")
	assert.Contains(t, got.LaunchWhy(false), "outbound remains open")
}

func TestOpenCodeFilteredNetworkUsesSharedPrerequisiteContract(t *testing.T) {
	axes := sandboxpolicy.ResolvedAxes{Network: sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{{
			CIDR: "192.0.2.0/24", Ports: []int{443},
		}},
	}}
	openCode := harness.MustGet(harness.OpenCodeName)

	err := ValidateFilteredNetworkHarnessSupport(
		openCode,
		sandboxpolicy.ImplementationTclaudeLayer,
		axes,
		FilteredNetworkPrerequisite{
			Detected: false,
			Detail:   "pasta is unavailable",
		},
	)
	require.NoError(t, err,
		"an unavailable prerequisite must retain widen-and-disclose behavior")

	err = ValidateFilteredNetworkHarnessSupport(
		openCode,
		sandboxpolicy.ImplementationTclaudeLayer,
		axes,
		FilteredNetworkPrerequisite{Detected: true},
	)
	require.NoError(t, err,
		"the pinned M3 boundary activates OpenCode on the shared gateway")

	err = ValidateFilteredNetworkHarnessSupport(
		harness.Default(),
		sandboxpolicy.ImplementationTclaudeLayer,
		axes,
		FilteredNetworkPrerequisite{Detected: true},
	)
	require.NoError(t, err)
}

// A base-tier pasta carries an explicitly private routed namespace but nothing
// that authors rows. The predicate is shared by the refusal and both posture
// selections precisely so those cannot disagree.
func TestFilteredNetworkPostureAvailableSeparatesTheTwoTiers(t *testing.T) {
	baseTier := FilteredNetworkPrerequisite{PrivateNamespaceDetected: true}
	fullTier := FilteredNetworkPrerequisite{
		Detected: true, PrivateNamespaceDetected: true,
	}
	unavailable := FilteredNetworkPrerequisite{}

	private := sandboxpolicy.NetworkRules{
		Mode:      sandboxpolicy.AccessModeOpen,
		Namespace: sandboxpolicy.NetworkNamespacePrivate,
	}
	list := sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{{
			CIDR: "192.0.2.0/24", Ports: []int{443},
		}},
	}
	privateWithDeny := sandboxpolicy.NetworkRules{
		Mode:      sandboxpolicy.AccessModeOpen,
		Namespace: sandboxpolicy.NetworkNamespacePrivate,
		Deny:      []sandboxpolicy.NetworkAllowEntry{{Host: "telemetry.example"}},
	}

	assert.True(t, FilteredNetworkPostureAvailable(baseTier, private))
	assert.False(t, FilteredNetworkPostureAvailable(baseTier, list),
		"an authored list needs the synthetic host-loopback mapping")
	assert.False(t, FilteredNetworkPostureAvailable(baseTier, privateWithDeny),
		"the base tier is deliberately scoped to the rowless private posture")

	assert.True(t, FilteredNetworkPostureAvailable(fullTier, private))
	assert.True(t, FilteredNetworkPostureAvailable(fullTier, list))
	assert.True(t, FilteredNetworkPostureAvailable(fullTier, privateWithDeny))

	assert.False(t, FilteredNetworkPostureAvailable(unavailable, private))
	assert.False(t, FilteredNetworkPostureAvailable(unavailable, list))
}

// The refusal must follow the same predicate: a base-tier host admits a private
// namespace, and an enforced private launch must not be disclosed as open.
func TestPrivateNetworkOnBaseTierPastaIsAdmittedAndDisclosedAsEnforced(t *testing.T) {
	axes := sandboxpolicy.ResolvedAxes{Network: sandboxpolicy.NetworkRules{
		Mode:      sandboxpolicy.AccessModeOpen,
		Namespace: sandboxpolicy.NetworkNamespacePrivate,
	}}
	probe := FilteredNetworkPrerequisite{
		PrivateNamespaceDetected: true,
		Detail:                   "this pasta predates the synthetic host-loopback controls",
	}
	require.NoError(t, ValidateFilteredNetworkHarnessSupport(
		harness.Default(), sandboxpolicy.ImplementationTclaudeLayer, axes, probe))

	why := probe.LaunchWhy(true)
	assert.Contains(t, why, "private-namespace tier")
	assert.Contains(t, why, "host loopback closed")
	assert.NotContains(t, why, "outbound remains open")

	notice := FilteredNetworkPrerequisiteNotice(probe, true)
	assert.Equal(t,
		sandboxpolicy.AccessNoticeEffectLaunchGated, notice.Effect)

	// The same probe under a launch that could not consume it stays honest.
	assert.Contains(t, probe.LaunchWhy(false), "outbound remains open")
	assert.Equal(t,
		sandboxpolicy.AccessNoticeEffectNotEnforced,
		FilteredNetworkPrerequisiteNotice(probe, false).Effect)
}

// A base-tier host authoring a real list keeps the historical widen-and-disclose
// behaviour rather than gaining a new refusal.
func TestListPolicyOnBaseTierPastaStillWidensAndDiscloses(t *testing.T) {
	axes := sandboxpolicy.ResolvedAxes{Network: sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{{
			CIDR: "192.0.2.0/24", Ports: []int{443},
		}},
	}}
	probe := FilteredNetworkPrerequisite{
		PrivateNamespaceDetected: true,
		Detail:                   "this pasta predates the synthetic host-loopback controls",
	}
	require.NoError(t, ValidateFilteredNetworkHarnessSupport(
		harness.Default(), sandboxpolicy.ImplementationTclaudeLayer, axes, probe),
		"a list policy must widen and disclose, not refuse")
	assert.False(t, FilteredNetworkPostureAvailable(probe, axes.Network))
	assert.Contains(t, probe.LaunchWhy(false), "outbound remains open")
}

func TestPrivateNetworkPrerequisiteRefusalNamesExactProbeFailure(t *testing.T) {
	axes := sandboxpolicy.ResolvedAxes{Network: sandboxpolicy.NetworkRules{
		Mode:      sandboxpolicy.AccessModeOpen,
		Namespace: sandboxpolicy.NetworkNamespacePrivate,
	}}
	err := ValidateFilteredNetworkHarnessSupport(
		harness.MustGet(harness.OpenCodeName),
		sandboxpolicy.ImplementationTclaudeLayer,
		axes,
		FilteredNetworkPrerequisite{
			Detail: "rootless pasta is required: executable file not found in $PATH",
		},
	)
	require.ErrorContains(t, err,
		"rootless pasta is required: executable file not found in $PATH")
	require.ErrorContains(t, err, `network.namespace "private"`)

	err = ValidateFilteredNetworkHarnessSupport(
		harness.MustGet(harness.OpenCodeName),
		sandboxpolicy.ImplementationTclaudeLayer,
		axes,
		FilteredNetworkPrerequisite{Detected: true},
	)
	require.NoError(t, err)
}

func TestSessionReplanRetainsFilteredProbeNoticeForPersistence(t *testing.T) {
	prior := []sandboxpolicy.AccessNotice{{
		Class:  sandboxpolicy.AccessNoticeClassDegradation,
		Axis:   "network",
		Reason: "no_mechanism",
		Effect: sandboxpolicy.AccessNoticeEffectNotEnforced,
		Detail: "network list remains open",
	}}
	current := appendFilteredNetworkPrerequisiteNotice(
		prior,
		true,
		sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeList},
		true,
		func() FilteredNetworkPrerequisite {
			return FilteredNetworkPrerequisite{
				Detected: true,
				Detail:   "namespace execution passed; executables found; gateway not verified",
			}
		},
	)
	persisted := sandboxpolicy.ReplaceAccessDegradationNotices(nil, current...)
	require.Len(t, persisted, 2)
	assert.Equal(t, "no_mechanism", persisted[0].Reason)
	assert.Equal(t, sandboxpolicy.AccessNoticeReasonFilteredPrerequisite, persisted[1].Reason)
	assert.Equal(t, sandboxpolicy.AccessNoticeEffectLaunchGated, persisted[1].Effect)
	assert.Contains(t, persisted[1].Detail, "prerequisite probe: detected")
	assert.Contains(t, persisted[1].Detail, "atomic nft policy")
	assert.NotContains(t, persisted[1].Detail, "outbound remains open")

	defaultAllowDeny := appendFilteredNetworkPrerequisiteNotice(
		nil,
		true,
		sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeOpen,
			Deny: []sandboxpolicy.NetworkAllowEntry{{CIDR: "192.0.2.0/24"}},
		},
		false,
		func() FilteredNetworkPrerequisite {
			return FilteredNetworkPrerequisite{
				Detected: true,
				Detail:   "namespace execution passed; verdict unavailable",
			}
		},
	)
	require.Len(t, defaultAllowDeny, 1)
	assert.Equal(t, sandboxpolicy.AccessNoticeEffectNotEnforced,
		defaultAllowDeny[0].Effect)
	assert.Contains(t, defaultAllowDeny[0].Detail, "filtered network rules")
	assert.Contains(t, defaultAllowDeny[0].Detail, "outbound remains open")
	assert.NotContains(t, defaultAllowDeny[0].Detail, "network allow list")

	unchanged := appendFilteredNetworkPrerequisiteNotice(
		prior,
		false,
		sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeList},
		false,
		func() FilteredNetworkPrerequisite {
			t.Fatal("non-outer launch must not run the filtered prerequisite probe")
			return FilteredNetworkPrerequisite{}
		},
	)
	assert.Equal(t, prior, unchanged)
}

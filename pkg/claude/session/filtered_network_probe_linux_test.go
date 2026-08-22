//go:build linux

package session

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
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
	inspectFilteredNetworkPasta = func(string) error { return nil }

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
	help := strings.Join(requiredFilteredNetworkPastaOptions, "\n")
	require.NoError(t, validatePastaCapabilities(help))
	require.ErrorContains(t, validatePastaCapabilities(help, true), "--netns")
	help += "\n--netns\n--netns-only"
	require.NoError(t, validatePastaCapabilities(help, true))

	help = strings.ReplaceAll(help, "--map-host-loopback", "--old--map-host-loopback")
	help = strings.ReplaceAll(help, "--gateway", "--old--gateway")
	help = strings.ReplaceAll(help, "--pid", "--pidfile")
	help = strings.ReplaceAll(help, "--netns", "--old--netns")
	help = strings.ReplaceAll(help, "--netns-only", "--old--netns-only")
	err := validatePastaCapabilities(help, true)
	require.ErrorContains(t, err, "--map-host-loopback")
	assert.ErrorContains(t, err, "--gateway")
	assert.ErrorContains(t, err, "--pid")
	assert.ErrorContains(t, err, "--netns")
	assert.ErrorContains(t, err, "--netns-only")
	assert.NotContains(t, err.Error(), "--map-guest-addr")
}

func TestFilteredNetworkExecutableResolutionUsesExactIdentityCapabilities(t *testing.T) {
	oldPath := filteredNetworkLookPath
	oldDefault := inspectFilteredNetworkPasta
	oldIdentity := inspectFilteredNetworkPastaIdentity
	t.Cleanup(func() {
		filteredNetworkLookPath = oldPath
		inspectFilteredNetworkPasta = oldDefault
		inspectFilteredNetworkPastaIdentity = oldIdentity
	})
	stubTrustedExecutableWalk(t)
	filteredNetworkLookPath = func(name string) (string, error) {
		return "/usr/bin/" + name, nil
	}
	defaultCalls, identityCalls := 0, 0
	inspectFilteredNetworkPasta = func(string) error {
		defaultCalls++
		return nil
	}
	inspectFilteredNetworkPastaIdentity = func(string) error {
		identityCalls++
		return nil
	}

	_, err := resolveFilteredNetworkExecutables(false)
	require.NoError(t, err)
	assert.Equal(t, 1, defaultCalls)
	assert.Zero(t, identityCalls)

	_, err = resolveFilteredNetworkExecutables(true)
	require.NoError(t, err)
	assert.Equal(t, 1, defaultCalls)
	assert.Equal(t, 1, identityCalls)
}

func TestResolveBwrapServerBinaryGatesCallerIdentityPastaMode(t *testing.T) {
	oldBwrapPath := lookPathBwrap
	oldBwrapProbe := probeBwrapIdentity
	oldFilteredPath := filteredNetworkLookPath
	oldIdentity := inspectFilteredNetworkPastaIdentity
	t.Cleanup(func() {
		lookPathBwrap = oldBwrapPath
		probeBwrapIdentity = oldBwrapProbe
		filteredNetworkLookPath = oldFilteredPath
		inspectFilteredNetworkPastaIdentity = oldIdentity
	})
	stubTrustedExecutableWalk(t)
	lookPathBwrap = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	probeBwrapIdentity = func(string, sandboxpolicy.NetworkPosture, sandboxpolicy.RootPosture) error {
		return nil
	}
	filteredNetworkLookPath = func(name string) (string, error) {
		return "/usr/bin/" + name, nil
	}
	inspectFilteredNetworkPastaIdentity = func(string) error {
		return errors.New("missing options: --netns, --netns-only")
	}

	_, err := resolveBwrapServerBinary(
		sandboxpolicy.NetworkFiltered, sandboxpolicy.RootConstructed, true)
	require.ErrorContains(t, err, "caller-identity namespace controls")
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
	require.ErrorContains(t, inspectPastaCapabilities("/trusted/pasta"), "deadline exceeded")

	filteredNetworkPastaProbeTimeout = 5 * time.Second
	filteredNetworkPastaHelpLimit = 8
	filteredNetworkPastaCommand = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/echo", "123456789")
	}
	require.ErrorContains(t, inspectPastaCapabilities("/trusted/pasta"), "output exceeds")
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
	inspectFilteredNetworkPasta = func(string) error {
		return errors.New("missing options: --map-host-loopback")
	}

	got := ProbeFilteredNetworkPrerequisite()
	require.False(t, got.Detected)
	assert.Contains(t, got.Detail, "pasta")
	assert.Contains(t, got.Detail, "--map-host-loopback")
	assert.Contains(t, got.LaunchWhy(false), "outbound remains open")
}

func TestFilteredNetworkProbeArgsBuildTheNamespaceShapeWithoutInSandboxCapability(t *testing.T) {
	for _, preserve := range []bool{false, true} {
		t.Run(strconv.FormatBool(preserve), func(t *testing.T) {
			args, err := tclaudeLayerProbeArgs(
				sandboxpolicy.NetworkFiltered, sandboxpolicy.RootConstructed, preserve)
			require.NoError(t, err)
			joined := strings.Join(args, " ")
			assert.Contains(t, joined, "--unshare-user")
			assert.Contains(t, joined, "--unshare-net --unshare-pid")
			if preserve {
				assert.NotContains(t, args, "--uid")
				assert.NotContains(t, args, "--gid")
				assert.Contains(t, args[len(args)-1], `test "$(id -u)" = `)
				assert.Contains(t, args[len(args)-1], `test "$(id -g)" = `)
			} else {
				assert.NotEqual(t, -1, indexOfBwrapTriplet(args, "--uid", "0"))
				assert.NotEqual(t, -1, indexOfBwrapTriplet(args, "--gid", "0"))
				assert.NotContains(t, args[len(args)-1], "id -u")
			}
			assert.NotContains(t, joined, "--cap-add")
			assert.NotContains(t, joined, "CAP_NET_ADMIN")
		})
	}
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

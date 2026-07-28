//go:build linux

package session

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

func TestFilteredNetworkPrerequisiteProbeNamesEveryBuildingBlock(t *testing.T) {
	oldBwrapPath := lookPathBwrap
	oldBwrapProbe := probeBwrap
	oldFilteredPath := filteredNetworkLookPath
	oldFilteredEval := filteredNetworkEvalSymlinks
	oldFilteredValidate := validateFilteredNetworkExecutable
	t.Cleanup(func() {
		lookPathBwrap = oldBwrapPath
		probeBwrap = oldBwrapProbe
		filteredNetworkLookPath = oldFilteredPath
		filteredNetworkEvalSymlinks = oldFilteredEval
		validateFilteredNetworkExecutable = oldFilteredValidate
	})
	lookPathBwrap = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	probeBwrap = func(_ string, posture sandboxpolicy.NetworkPosture) error {
		assert.Equal(t, sandboxpolicy.NetworkIsolatedWithAgentd, posture)
		return nil
	}
	filteredNetworkLookPath = func(name string) (string, error) {
		return "/usr/bin/" + name, nil
	}
	filteredNetworkEvalSymlinks = func(path string) (string, error) { return path, nil }
	validateFilteredNetworkExecutable = func(string) error { return nil }

	got := ProbeFilteredNetworkPrerequisite()
	require.True(t, got.Detected)
	assert.Contains(t, got.Detail, "bubblewrap")
	assert.Contains(t, got.Detail, "user/network namespace")
	assert.Contains(t, got.Detail, "pasta")
	assert.Contains(t, got.Detail, "nft")
	assert.Contains(t, got.Detail, "gated launch boundary")
	assert.Contains(t, got.LaunchWhy(true), "atomic nft policy")
	assert.NotContains(t, got.LaunchWhy(true), "outbound remains open")
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
	lookPathBwrap = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	probeBwrap = func(string, sandboxpolicy.NetworkPosture) error { return nil }
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

func TestOpenCodeFilteredNetworkRefusesOnlyAfterPrerequisitesResolve(t *testing.T) {
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
	require.ErrorContains(t, err, "real-OpenCode M3 smoke")

	err = ValidateFilteredNetworkHarnessSupport(
		harness.Default(),
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

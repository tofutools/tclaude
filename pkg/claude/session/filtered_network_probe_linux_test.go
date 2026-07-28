//go:build linux

package session

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestFilteredNetworkPrerequisiteProbeNamesEveryBuildingBlock(t *testing.T) {
	oldBwrapPath := lookPathBwrap
	oldBwrapProbe := probeBwrap
	oldFilteredPath := filteredNetworkLookPath
	t.Cleanup(func() {
		lookPathBwrap = oldBwrapPath
		probeBwrap = oldBwrapProbe
		filteredNetworkLookPath = oldFilteredPath
	})
	lookPathBwrap = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	probeBwrap = func(_ string, posture sandboxpolicy.NetworkPosture) error {
		assert.Equal(t, sandboxpolicy.NetworkIsolatedWithAgentd, posture)
		return nil
	}
	filteredNetworkLookPath = func(name string) (string, error) {
		return "/usr/bin/" + name, nil
	}

	got := ProbeFilteredNetworkPrerequisite()
	require.True(t, got.Detected)
	assert.Contains(t, got.Detail, "bubblewrap")
	assert.Contains(t, got.Detail, "user/network namespace")
	assert.Contains(t, got.Detail, "pasta")
	assert.Contains(t, got.Detail, "nft")
	assert.Contains(t, got.Detail, "not verified in M2a")
	assert.Contains(t, got.LaunchWhy(), "not enabled yet")
	assert.Contains(t, got.LaunchWhy(), "remains unenforced")
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
	assert.Contains(t, got.LaunchWhy(), "unavailable")
	assert.Contains(t, got.LaunchWhy(), "outbound remains open")
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
	assert.Contains(t, persisted[1].Detail, "prerequisite probe: detected")
	assert.Contains(t, persisted[1].Detail, "outbound remains open")

	unchanged := appendFilteredNetworkPrerequisiteNotice(
		prior,
		false,
		sandboxpolicy.NetworkRules{Mode: sandboxpolicy.AccessModeList},
		func() FilteredNetworkPrerequisite {
			t.Fatal("non-outer launch must not run the filtered prerequisite probe")
			return FilteredNetworkPrerequisite{}
		},
	)
	assert.Equal(t, prior, unchanged)
}

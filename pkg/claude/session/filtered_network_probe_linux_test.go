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
	require.True(t, got.Available)
	assert.Contains(t, got.Detail, "bubblewrap")
	assert.Contains(t, got.Detail, "user/network namespaces")
	assert.Contains(t, got.Detail, "pasta")
	assert.Contains(t, got.Detail, "nftables")
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
	require.False(t, got.Available)
	assert.Contains(t, got.Detail, "pasta")
	assert.Contains(t, got.LaunchWhy(), "unavailable")
	assert.Contains(t, got.LaunchWhy(), "outbound remains open")
}

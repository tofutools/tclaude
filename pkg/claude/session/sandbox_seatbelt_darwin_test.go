//go:build darwin

package session

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestResolveTclaudeLayerDarwinRefusesUnsupportedNetworkBeforeProbe(t *testing.T) {
	oldStat := statDarwinSeatbelt
	oldProbe := probeDarwinSeatbelt
	t.Cleanup(func() {
		statDarwinSeatbelt = oldStat
		probeDarwinSeatbelt = oldProbe
	})
	statDarwinSeatbelt = func(string) (os.FileInfo, error) {
		t.Fatal("unsupported posture must refuse before inspecting sandbox-exec")
		return nil, nil
	}
	probeDarwinSeatbelt = func(string) error {
		t.Fatal("unsupported posture must refuse before probing sandbox-exec")
		return nil
	}

	_, verdict, err := ResolveTclaudeLayer(sandboxpolicy.NetworkFiltered)
	require.ErrorContains(t, err, "does not support reserved filtered networking")
	assert.Equal(t, "off", verdict.State)
	assert.Equal(t, "tclaude-layer unavailable", verdict.Source)
}

func TestResolveTclaudeLayerDarwinAcceptsIsolatedNetwork(t *testing.T) {
	oldStat := statDarwinSeatbelt
	oldProbe := probeDarwinSeatbelt
	t.Cleanup(func() {
		statDarwinSeatbelt = oldStat
		probeDarwinSeatbelt = oldProbe
	})

	executable, err := os.Stat(os.Args[0])
	require.NoError(t, err)
	statDarwinSeatbelt = func(path string) (os.FileInfo, error) {
		assert.Equal(t, darwinSeatbeltExecutable, path)
		return executable, nil
	}
	probed := false
	probeDarwinSeatbelt = func(path string) error {
		probed = true
		assert.Equal(t, darwinSeatbeltExecutable, path)
		return nil
	}

	binary, verdict, err := ResolveTclaudeLayer(sandboxpolicy.NetworkIsolatedWithAgentd)
	require.NoError(t, err)
	assert.Equal(t, darwinSeatbeltExecutable, binary)
	assert.True(t, probed)
	assert.Equal(t, "on", verdict.State)
	assert.Contains(t, verdict.Source, "isolated network")
}

func TestResolveTclaudeLayerDarwinRefusesMissingOrBrokenSeatbelt(t *testing.T) {
	oldStat := statDarwinSeatbelt
	oldProbe := probeDarwinSeatbelt
	t.Cleanup(func() {
		statDarwinSeatbelt = oldStat
		probeDarwinSeatbelt = oldProbe
	})

	statDarwinSeatbelt = func(string) (os.FileInfo, error) {
		return nil, errors.New("not found")
	}
	_, _, err := ResolveTclaudeLayer(sandboxpolicy.NetworkHostOpen)
	require.ErrorContains(t, err, darwinSeatbeltExecutable)

	executable, statErr := os.Stat(os.Args[0])
	require.NoError(t, statErr)
	statDarwinSeatbelt = func(string) (os.FileInfo, error) {
		return executable, nil
	}
	probeDarwinSeatbelt = func(string) error {
		return errors.New("operation unexpectedly succeeded")
	}
	_, _, err = ResolveTclaudeLayer(sandboxpolicy.NetworkHostOpen)
	require.ErrorContains(t, err, "deny-write capability")
}

func TestTclaudeLayerHostAvailabilityDarwinUsesSeatbeltCapability(t *testing.T) {
	oldStat := statDarwinSeatbelt
	oldProbe := probeDarwinSeatbelt
	t.Cleanup(func() {
		statDarwinSeatbelt = oldStat
		probeDarwinSeatbelt = oldProbe
	})

	executable, err := os.Stat(os.Args[0])
	require.NoError(t, err)
	statDarwinSeatbelt = func(path string) (os.FileInfo, error) {
		assert.Equal(t, darwinSeatbeltExecutable, path)
		return executable, nil
	}
	probed := false
	probeDarwinSeatbelt = func(path string) error {
		probed = true
		assert.Equal(t, darwinSeatbeltExecutable, path)
		return nil
	}
	require.NoError(t, TclaudeLayerHostAvailability())
	assert.True(t, probed, "availability must execute the same deny-write probe as launch")

	probeDarwinSeatbelt = func(string) error {
		return errors.New("deny probe failed")
	}
	require.ErrorContains(t, TclaudeLayerHostAvailability(), "deny-write capability")
}

func TestDarwinSeatbeltCapabilityProbeHasDeadline(t *testing.T) {
	oldRun := runDarwinSeatbeltProbe
	t.Cleanup(func() { runDarwinSeatbeltProbe = oldRun })
	t.Setenv("TMPDIR", "/private/var/folders/ab/runtime/T")

	runDarwinSeatbeltProbe = func(
		ctx context.Context,
		_, _, _ string,
	) ([]byte, error) {
		deadline, ok := ctx.Deadline()
		require.True(t, ok, "the dashboard/launch capability predicate must be bounded")
		remaining := time.Until(deadline)
		assert.Greater(t, remaining, 4*time.Second)
		assert.LessOrEqual(t, remaining, darwinSeatbeltProbeTimeout)
		return nil, context.DeadlineExceeded
	}

	err := probeDarwinSeatbeltCapability(darwinSeatbeltExecutable)
	require.ErrorContains(t, err, "timed out after 5s")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestTclaudeLayerDarwinVerdictIsPlatformSpecificAndUnverified(t *testing.T) {
	hostOpen := TclaudeLayerLaunchOSSandbox(sandboxpolicy.NetworkHostOpen)
	assert.Equal(t, "on", hostOpen.State)
	assert.Equal(t,
		"tclaude-layer (Seatbelt/sandbox-exec; host network)",
		hostOpen.Source,
	)
	assert.True(t, hostOpen.Unverified)
	assert.NotContains(t, hostOpen.Source, "bubblewrap")

	isolated := TclaudeLayerLaunchOSSandbox(sandboxpolicy.NetworkIsolatedWithAgentd)
	assert.Equal(t, "on", isolated.State)
	assert.Equal(t,
		"tclaude-layer (Seatbelt/sandbox-exec; isolated network; "+
			"host loopback/IDE bridge unavailable; agentd socket allowlisted)",
		isolated.Source,
	)
	assert.True(t, isolated.Unverified)
}

func TestTclaudeLayerDarwinCommandCarriesFullAgentdSocketFloor(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	command, err := tclaudeLayerCommand(
		darwinSeatbeltExecutable,
		nil,
		nil,
		nil,
		nil,
		sandboxpolicy.AgentdSocketFloor(),
		sandboxpolicy.MountPlan{NetworkPosture: sandboxpolicy.NetworkIsolatedWithAgentd},
		"true",
	)
	require.NoError(t, err)
	for _, socket := range sandboxpolicy.AgentdSocketFloor() {
		assert.Containsf(t, command, socket, "missing rendered agentd socket floor entry %s", socket)
	}
}

func TestDarwinSeatbeltRuntimeTempDirRefusesNonstandardCarveout(t *testing.T) {
	t.Setenv("TMPDIR", "/Users/dev/operator-controlled")
	_, err := darwinSeatbeltRuntimeTempDir()
	require.ErrorContains(t, err, "only carves the standard /private/var/folders runtime tree")

	t.Setenv("TMPDIR", "/private/var/folders/ab/runtime/T")
	got, err := darwinSeatbeltRuntimeTempDir()
	require.NoError(t, err)
	assert.Equal(t, "/private/var/folders/ab/runtime/T", got)
}

//go:build darwin

package session

import (
	"errors"
	"os"
	"testing"

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

	_, verdict, err := ResolveTclaudeLayer(sandboxpolicy.NetworkIsolatedWithAgentd)
	require.ErrorContains(t, err, "does not yet support network_access none")
	assert.Equal(t, "off", verdict.State)
	assert.Equal(t, "tclaude-layer unavailable", verdict.Source)
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

func TestTclaudeLayerDarwinVerdictIsPlatformSpecificAndUnverified(t *testing.T) {
	got := TclaudeLayerLaunchOSSandbox(sandboxpolicy.NetworkHostOpen)
	assert.Equal(t, "on", got.State)
	assert.Equal(t,
		"tclaude-layer (Seatbelt/sandbox-exec; filesystem policy enforced; "+
			"host network and ambient Unix sockets reachable; no mount namespace; "+
			"hidden paths remain enumerable)",
		got.Source,
	)
	assert.True(t, got.Unverified)
	assert.NotContains(t, got.Source, "bubblewrap")
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

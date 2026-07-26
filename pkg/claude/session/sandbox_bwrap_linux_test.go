//go:build linux

package session

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

func TestResolveTclaudeLayerRefusesMissingBwrapAndRecordsOffVerdict(t *testing.T) {
	oldLookPath := lookPathBwrap
	oldProbe := probeBwrap
	t.Cleanup(func() {
		lookPathBwrap = oldLookPath
		probeBwrap = oldProbe
	})
	lookPathBwrap = func(string) (string, error) {
		return "", errors.New("executable file not found")
	}

	_, verdict, err := ResolveTclaudeLayer(sandboxpolicy.NetworkHostOpen)
	require.ErrorContains(t, err, "requires bubblewrap (`bwrap`) on PATH")
	assert.Equal(t, "off", verdict.State)
	assert.Equal(t, "tclaude-layer unavailable", verdict.Source)

	row := toRow(&SessionState{
		ID:              "refused",
		OSSandboxState:  verdict.State,
		OSSandboxSource: verdict.Source,
	})
	assert.Equal(t, "off", row.OSSandboxState)
	assert.Equal(t, "tclaude-layer unavailable", row.OSSandboxSource)
}

func TestResolveTclaudeLayerRefusesUnavailableUserNamespace(t *testing.T) {
	oldLookPath := lookPathBwrap
	oldProbe := probeBwrap
	t.Cleanup(func() {
		lookPathBwrap = oldLookPath
		probeBwrap = oldProbe
	})
	lookPathBwrap = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	probeBwrap = func(string, sandboxpolicy.NetworkPosture) error {
		return errors.New("operation not permitted")
	}

	_, verdict, err := ResolveTclaudeLayer(sandboxpolicy.NetworkHostOpen)
	require.ErrorContains(t, err, "unprivileged user namespaces may be unavailable")
	assert.Equal(t, "off", verdict.State)
}

func TestResolveTclaudeLayerRefusesUnavailableIsolatedNamespaces(t *testing.T) {
	oldLookPath := lookPathBwrap
	oldProbe := probeBwrap
	t.Cleanup(func() {
		lookPathBwrap = oldLookPath
		probeBwrap = oldProbe
	})
	lookPathBwrap = func(string) (string, error) { return "/usr/bin/bwrap", nil }
	var probed []sandboxpolicy.NetworkPosture
	probeBwrap = func(_ string, posture sandboxpolicy.NetworkPosture) error {
		probed = append(probed, posture)
		if posture == sandboxpolicy.NetworkIsolatedWithAgentd {
			return errors.New("operation not permitted")
		}
		return nil
	}

	_, _, err := ResolveTclaudeLayer(sandboxpolicy.NetworkHostOpen)
	require.NoError(t, err)
	_, verdict, err := ResolveTclaudeLayer(sandboxpolicy.NetworkIsolatedWithAgentd)
	require.ErrorContains(t, err, "mount, network, and PID namespaces")
	require.ErrorContains(t, err, "read-only remount support")
	assert.Equal(t, "off", verdict.State)
	assert.Equal(t, []sandboxpolicy.NetworkPosture{
		sandboxpolicy.NetworkHostOpen,
		sandboxpolicy.NetworkIsolatedWithAgentd,
	}, probed)
}

func TestTclaudeLayerProbeExercisesReadOnlyRemountSemantics(t *testing.T) {
	for _, posture := range []sandboxpolicy.NetworkPosture{
		sandboxpolicy.NetworkHostOpen,
		sandboxpolicy.NetworkIsolatedWithAgentd,
	} {
		t.Run(posture.String(), func(t *testing.T) {
			args, err := tclaudeLayerProbeArgs(posture)
			require.NoError(t, err)

			tmpfs := indexOfBwrapTriplet(args, "--tmpfs", "/tmp")
			childBind := indexOfBwrapTriplet(args, "--ro-bind", "/dev/null")
			remount := indexOfBwrapTriplet(args, "--remount-ro", "/tmp")
			require.NotEqual(t, -1, tmpfs)
			require.NotEqual(t, -1, childBind)
			require.NotEqual(t, -1, remount)
			assert.Less(t, tmpfs, childBind)
			assert.Less(t, childBind, remount)
			assert.Contains(t, args[len(args)-1], "! touch /tmp/.tclaude-remount-write")
		})
	}
}

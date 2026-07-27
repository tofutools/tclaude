//go:build linux

package session

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/agentipc"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

// TestStackedSandboxHostSmoke is a hard CI gate over the production live-probe
// path. It uses the exact pinned Claude CLI/embedded SRT and Codex engines and
// both outer postures; dependency/version checks alone cannot make the test
// pass.
func TestStackedSandboxHostSmoke(t *testing.T) {
	if os.Getenv("TCLAUDE_STACKED_SANDBOX_SMOKE") != "1" {
		t.Skip("set TCLAUDE_STACKED_SANDBOX_SMOKE=1 with pinned claude, codex, and bwrap on PATH")
	}
	tclaudeBinary := os.Getenv("TCLAUDE_SANDBOX_V2_TCLAUDE_BINARY")
	require.NotEmpty(t, tclaudeBinary)
	previousRelay := tclaudeLayerRelayPrefix
	tclaudeLayerRelayPrefix = func() string {
		return clcommon.ShellQuoteArg(tclaudeBinary) +
			" session " + tclaudeLayerWinchRelayCommand
	}
	t.Cleanup(func() { tclaudeLayerRelayPrefix = previousRelay })
	prepareStackedSmokeControlPlane(t)
	cwd := t.TempDir()
	var err error
	cwd, err = filepath.EvalSymlinks(cwd)
	require.NoError(t, err)

	for _, tc := range []struct {
		name    string
		posture sandboxpolicy.NetworkPosture
		access  sandboxpolicy.NetworkAccess
	}{
		{name: "host-open", posture: sandboxpolicy.NetworkHostOpen, access: sandboxpolicy.NetworkAccessInherit},
		{name: "isolated", posture: sandboxpolicy.NetworkIsolatedWithAgentd, access: sandboxpolicy.NetworkAccessNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := sandboxpolicy.EmptySnapshot()
			snapshot.Effective.NetworkAccess = tc.access
			for _, harnessName := range []string{harness.DefaultName, harness.CodexName} {
				t.Run(harnessName, func(t *testing.T) {
					h := harness.MustGet(harnessName)
					binary, _, err := ResolveTclaudeLayer(tc.posture)
					require.NoError(t, err)
					spec, err := BuildTclaudeLayerLaunchSpec(TclaudeLayerLaunchInput{
						HarnessName: h.Name,
						Cwd:         cwd,
						Snapshot:    &snapshot,
					})
					require.NoError(t, err)
					proof, probeErr := ProbeStackedSandbox(binary, spec, h, cwd)
					require.NoError(t, probeErr)
					require.NotNil(t, proof)
					bound, bindErr := WrapTclaudeLayerStackedSpec(
						binary,
						spec,
						proof.ManifestPath,
						proof.ManifestSHA256,
						proof.ReadyPath,
						true,
						clcommon.ShellQuoteArg(proof.Executable.Path)+" --version",
					)
					require.NoError(t, bindErr)
					boundOutput, bindErr := exec.Command("/bin/sh", "-c", bound).CombinedOutput()
					require.NoError(t, bindErr, string(boundOutput))
					require.NoError(t, WaitForStackedBindingReadiness(proof.ReadyPath))
					proof.Cleanup()
					verdict := StackedLaunchOSSandbox(h, tc.posture)
					require.Equal(t, "on", verdict.State)
					require.Contains(t, verdict.Source, h.NestedSandbox.MechanismName())
				})
			}
		})
	}
}

// The production launch has already opened tclaude's private data directory
// and an isolated agent launch has a real agentd socket. The direct smoke
// bypasses those earlier seams, so materialize only that production
// prerequisite; the nested engines remain the real pinned binaries.
func prepareStackedSmokeControlPlane(t *testing.T) {
	t.Helper()
	require.NoError(t, os.MkdirAll(tclcommon.TclaudeDataDir(), 0o700))
	socketPath := agentipc.CanonicalSocketPath()
	if _, err := os.Lstat(socketPath); err == nil {
		return
	} else {
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	require.NoError(t, os.MkdirAll(filepath.Dir(socketPath), 0o700))
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	})
}

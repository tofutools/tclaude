//go:build linux

package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/harness"
)

// TestStackedSandboxHostSmoke is a hard CI gate over the production live-probe
// path. It uses the real pinned SRT and Codex engines and both outer postures;
// dependency/version checks alone cannot make the test pass.
func TestStackedSandboxHostSmoke(t *testing.T) {
	if os.Getenv("TCLAUDE_STACKED_SANDBOX_SMOKE") != "1" {
		t.Skip("set TCLAUDE_STACKED_SANDBOX_SMOKE=1 with pinned srt, codex, and bwrap on PATH")
	}
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
					require.NoError(t, ProbeStackedSandbox(binary, spec, h, cwd))
					verdict := StackedLaunchOSSandbox(h, tc.posture)
					require.Equal(t, "on", verdict.State)
					require.Contains(t, verdict.Source, h.NestedSandbox.MechanismName())
				})
			}
		})
	}
}
